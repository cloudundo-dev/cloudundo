package awscli

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/url"
	"os"
	"os/exec"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
)

// Runner executes one aws CLI call.
type Runner interface {
	Run(ctx context.Context, extraEnv []string, args ...string) ([]byte, error)
}

// Error is a failed aws CLI call.
type Error struct {
	Args   []string
	Code   int
	Stderr string
}

func (e *Error) Error() string {
	msg := strings.TrimSpace(e.Stderr)
	if msg == "" {
		msg = "exit code " + strconv.Itoa(e.Code)
	}
	return fmt.Sprintf("aws %s: %s", strings.Join(e.Args, " "), msg)
}

// Has reports whether the AWS error text mentions s (an error code such as
// "NoSuchBucket").
func Has(err error, s string) bool {
	var ae *Error
	return errors.As(err, &ae) && strings.Contains(ae.Stderr, s)
}

// Exec runs the real aws binary.
type Exec struct {
	Bin string // default "aws"
	// Timeout bounds one call; a bucket copy (s3 sync) gets SyncTimeout.
	// Zero means the defaults below.
	Timeout     time.Duration
	SyncTimeout time.Duration
	// Progress, when set, hears about calls that take a while, so a slow
	// network does not look like a hang.
	Progress io.Writer
}

// Defaults for Exec. A hung connection fails (and so blocks the command)
// instead of waiting forever; the hook timeout in agents is 10 minutes.
const (
	DefaultCallTimeout = 2 * time.Minute
	DefaultSyncTimeout = 9 * time.Minute
)

// slowAfter is when a call counts as slow; a variable for tests.
var slowAfter = 5 * time.Second

// Run executes aws with args.
func (x Exec) Run(ctx context.Context, extraEnv []string, args ...string) ([]byte, error) {
	bin := x.Bin
	if bin == "" {
		bin = "aws"
	}
	limit := x.Timeout
	if limit <= 0 {
		limit = DefaultCallTimeout
	}
	if len(args) >= 2 && args[0] == "s3" && args[1] == "sync" {
		limit = x.SyncTimeout
		if limit <= 0 {
			limit = DefaultSyncTimeout
		}
	}
	ctx, cancel := context.WithTimeout(ctx, limit)
	defer cancel()

	cmd := exec.CommandContext(ctx, bin, args...)
	cmd.Env = append(os.Environ(), extraEnv...)
	// Never let the CLI open a pager or prompt while a hook is waiting.
	cmd.Env = append(cmd.Env, "AWS_PAGER=", "AWS_CLI_AUTO_PROMPT=off")
	var out, errb bytes.Buffer
	cmd.Stdout, cmd.Stderr = &out, &errb
	// The aws CLI may leave child processes holding its output open after
	// it is killed; do not wait for them.
	cmd.WaitDelay = 5 * time.Second

	done := make(chan struct{})
	var reporting sync.WaitGroup
	if x.Progress != nil {
		reporting.Add(1)
		go func(after time.Duration) {
			defer reporting.Done()
			x.report(done, args, after)
		}(slowAfter)
	}
	err := cmd.Run()
	close(done)
	reporting.Wait()
	if err != nil {
		code := -1
		var ee *exec.ExitError
		if errors.As(err, &ee) {
			code = ee.ExitCode()
		}
		if ctx.Err() == context.DeadlineExceeded {
			errb.WriteString(fmt.Sprintf("AWS did not answer within %s; check your network and try again", limit))
		} else if code == -1 {
			errb.WriteString(err.Error())
		}
		return out.Bytes(), &Error{Args: args, Code: code, Stderr: errb.String()}
	}
	return out.Bytes(), nil
}

// report says which call is slow, then keeps saying it is still waiting.
func (x Exec) report(done <-chan struct{}, args []string, after time.Duration) {
	what := "aws " + strings.Join(args, " ")
	if len(what) > 120 {
		what = what[:117] + "..."
	}
	start := time.Now()
	t := time.NewTimer(after)
	defer t.Stop()
	for first := true; ; first = false {
		select {
		case <-done:
			return
		case <-t.C:
		}
		if first {
			fmt.Fprintf(x.Progress, "cloudundo: waiting for AWS: %s\n", what)
		} else {
			fmt.Fprintf(x.Progress, "cloudundo: still waiting for AWS (%s)\n", time.Since(start).Round(time.Second))
		}
		t.Reset(15 * time.Second)
	}
}

// Client calls S3 through the CLI for one Conn.
type Client struct {
	R Runner
	C Conn
}

func (c Client) call(ctx context.Context, args ...string) ([]byte, error) {
	full := append(append([]string{}, args...), c.C.globalArgs()...)
	full = append(full, "--output", "json")
	return c.R.Run(ctx, c.C.environ(), full...)
}

// VersioningStatus returns "", "Enabled" or "Suspended".
func (c Client) VersioningStatus(ctx context.Context, bucket string) (string, error) {
	out, err := c.call(ctx, "s3api", "get-bucket-versioning", "--bucket", bucket)
	if err != nil {
		return "", err
	}
	var v struct{ Status string }
	if len(bytes.TrimSpace(out)) > 0 {
		if err := json.Unmarshal(out, &v); err != nil {
			return "", fmt.Errorf("read versioning of %s: %w", bucket, err)
		}
	}
	return v.Status, nil
}

// EnableVersioning turns versioning on. It cannot be turned off again,
// only suspended.
func (c Client) EnableVersioning(ctx context.Context, bucket string) error {
	_, err := c.call(ctx, "s3api", "put-bucket-versioning", "--bucket", bucket,
		"--versioning-configuration", "Status=Enabled")
	return err
}

// Lifecycle is a bucket's lifecycle configuration, kept as raw JSON rules
// so writing it back never drops a field cloudundo does not know about.
type Lifecycle struct {
	Rules                              []json.RawMessage `json:"Rules"`
	TransitionDefaultMinimumObjectSize string            `json:"TransitionDefaultMinimumObjectSize,omitempty"`
}

// GetLifecycle returns the configuration; ok is false when there is none.
func (c Client) GetLifecycle(ctx context.Context, bucket string) (Lifecycle, bool, error) {
	out, err := c.call(ctx, "s3api", "get-bucket-lifecycle-configuration", "--bucket", bucket)
	if Has(err, "NoSuchLifecycleConfiguration") {
		return Lifecycle{}, false, nil
	}
	if err != nil {
		return Lifecycle{}, false, err
	}
	var l Lifecycle
	if err := json.Unmarshal(out, &l); err != nil {
		return Lifecycle{}, false, fmt.Errorf("read lifecycle of %s: %w", bucket, err)
	}
	return l, true, nil
}

// PutLifecycle replaces the configuration with l.
func (c Client) PutLifecycle(ctx context.Context, bucket string, l Lifecycle) error {
	body, err := json.Marshal(struct {
		Rules []json.RawMessage `json:"Rules"`
	}{l.Rules})
	if err != nil {
		return err
	}
	args := []string{"s3api", "put-bucket-lifecycle-configuration", "--bucket", bucket,
		"--lifecycle-configuration", string(body)}
	if l.TransitionDefaultMinimumObjectSize != "" {
		args = append(args, "--transition-default-minimum-object-size", l.TransitionDefaultMinimumObjectSize)
	}
	_, err = c.call(ctx, args...)
	return err
}

// Version is one object version or delete marker.
type Version struct {
	Key          string
	VersionID    string
	IsLatest     bool
	LastModified time.Time
	DeleteMarker bool
	Size         int64
}

// ListVersions returns every version and delete marker under prefix,
// newest first per key.
func (c Client) ListVersions(ctx context.Context, bucket, prefix string) ([]Version, error) {
	args := []string{"s3api", "list-object-versions", "--bucket", bucket}
	if prefix != "" {
		args = append(args, "--prefix", prefix)
	}
	out, err := c.call(ctx, args...)
	if err != nil {
		return nil, err
	}
	type entry struct {
		Key          string
		VersionId    string
		IsLatest     bool
		LastModified time.Time
		Size         int64
	}
	var r struct {
		Versions      []entry
		DeleteMarkers []entry
	}
	if len(bytes.TrimSpace(out)) > 0 {
		if err := json.Unmarshal(out, &r); err != nil {
			return nil, fmt.Errorf("read versions of s3://%s/%s: %w", bucket, prefix, err)
		}
	}
	var vs []Version
	for _, e := range r.Versions {
		vs = append(vs, Version{e.Key, e.VersionId, e.IsLatest, e.LastModified, false, e.Size})
	}
	for _, e := range r.DeleteMarkers {
		vs = append(vs, Version{e.Key, e.VersionId, e.IsLatest, e.LastModified, true, 0})
	}
	sort.SliceStable(vs, func(i, j int) bool {
		if vs[i].Key != vs[j].Key {
			return vs[i].Key < vs[j].Key
		}
		return vs[i].LastModified.After(vs[j].LastModified)
	})
	return vs, nil
}

// DeleteVersion removes one specific version. cloudundo only ever uses
// this on delete markers, which removes the "deleted" state, not data.
func (c Client) DeleteVersion(ctx context.Context, bucket, key, versionID string) error {
	_, err := c.call(ctx, "s3api", "delete-object", "--bucket", bucket, "--key", key, "--version-id", versionID)
	return err
}

// RestoreVersion makes an older version current again by copying it over
// the key. Nothing is deleted: the newer versions stay in history.
func (c Client) RestoreVersion(ctx context.Context, bucket, key, versionID string) error {
	src := bucket + "/" + (&url.URL{Path: key}).EscapedPath() + "?versionId=" + url.QueryEscape(versionID)
	_, err := c.call(ctx, "s3api", "copy-object", "--bucket", bucket, "--key", key, "--copy-source", src)
	return err
}

// BucketExists reports whether bucket exists and is reachable.
func (c Client) BucketExists(ctx context.Context, bucket string) (bool, error) {
	_, err := c.call(ctx, "s3api", "head-bucket", "--bucket", bucket)
	if err == nil {
		return true, nil
	}
	if Has(err, "Not Found") || Has(err, "NoSuchBucket") || Has(err, "404") {
		return false, nil
	}
	return false, err
}

// BucketSummary counts the current objects under prefix ("" = whole
// bucket) and their total size.
func (c Client) BucketSummary(ctx context.Context, bucket, prefix string) (count int, size int64, err error) {
	args := []string{"s3api", "list-objects-v2", "--bucket", bucket,
		"--query", "{count: length(Contents || `[]`), size: sum(Contents[].Size || `[]`)}"}
	if prefix != "" {
		args = append(args, "--prefix", prefix)
	}
	out, err := c.call(ctx, args...)
	if err != nil {
		return 0, 0, err
	}
	var r struct {
		Count int     `json:"count"`
		Size  float64 `json:"size"`
	}
	if err := json.Unmarshal(out, &r); err != nil {
		return 0, 0, fmt.Errorf("read size of %s: %w", bucket, err)
	}
	return r.Count, int64(r.Size), nil
}

// AccountID returns the caller's AWS account.
func (c Client) AccountID(ctx context.Context) (string, error) {
	out, err := c.call(ctx, "sts", "get-caller-identity")
	if err != nil {
		return "", err
	}
	var r struct{ Account string }
	if err := json.Unmarshal(out, &r); err != nil || r.Account == "" {
		return "", fmt.Errorf("read account id: %v", err)
	}
	return r.Account, nil
}

// BucketRegion returns the bucket's region.
func (c Client) BucketRegion(ctx context.Context, bucket string) (string, error) {
	out, err := c.call(ctx, "s3api", "get-bucket-location", "--bucket", bucket)
	if err != nil {
		return "", err
	}
	var r struct{ LocationConstraint *string }
	if err := json.Unmarshal(out, &r); err != nil {
		return "", fmt.Errorf("read region of %s: %w", bucket, err)
	}
	switch {
	case r.LocationConstraint == nil || *r.LocationConstraint == "":
		return "us-east-1", nil
	case *r.LocationConstraint == "EU":
		return "eu-west-1", nil
	}
	return *r.LocationConstraint, nil
}

// CreateBucket creates a private bucket in region.
func (c Client) CreateBucket(ctx context.Context, bucket, region string) error {
	args := []string{"s3api", "create-bucket", "--bucket", bucket}
	if region != "" && region != "us-east-1" {
		args = append(args, "--create-bucket-configuration", "LocationConstraint="+region)
	}
	in := c
	in.C.Region = region
	if _, err := in.call(ctx, args...); err != nil {
		return err
	}
	_, err := in.call(ctx, "s3api", "put-public-access-block", "--bucket", bucket,
		"--public-access-block-configuration",
		"BlockPublicAcls=true,IgnorePublicAcls=true,BlockPublicPolicy=true,RestrictPublicBuckets=true")
	return err
}

// Sync copies everything from src to dst (both s3:// URIs).
func (c Client) Sync(ctx context.Context, src, dst string) error {
	_, err := c.call(ctx, "s3", "sync", src, dst, "--only-show-errors")
	return err
}
