// Package undo puts S3 data back the way it was before a protected
// command ran.
//
// Undo never deletes data. It removes delete markers (which only hide
// data) or copies an older version back on top, so an undo can itself be
// undone from the same version history.
package undo

import (
	"context"
	"fmt"
	"sort"
	"strings"

	"github.com/cloudundo/cloudundo/internal/awscli"
	"github.com/cloudundo/cloudundo/internal/journal"
)

// ClientFor returns a CLI client for a Conn.
type ClientFor func(awscli.Conn) awscli.Client

// Action is one restore step.
type Action struct {
	Desc string
	do   func(context.Context) error
}

// Plan is everything undo would do for one journal entry.
type Plan struct {
	Actions []Action
	// Notes explain what will be left as it is, and why.
	Notes []string
	// Pending means undo is not finished after these actions (a table is
	// still being restored); running undo again later completes it.
	Pending bool
}

// Apply runs the actions in order and stops at the first failure.
// It returns how many succeeded.
func (p Plan) Apply(ctx context.Context) (int, error) {
	for i, a := range p.Actions {
		if err := a.do(ctx); err != nil {
			return i, fmt.Errorf("%s: %w", a.Desc, err)
		}
	}
	return len(p.Actions), nil
}

// For builds the plan for an entry without changing anything. Applying
// the plan may record progress in e (a restored table's ARN), so callers
// should save e afterwards. dataDir is where undo may save the current
// values of DynamoDB items before putting older ones back.
func For(ctx context.Context, e *journal.Entry, clientFor ClientFor, dataDir string) (Plan, error) {
	var p Plan
	if len(e.Undo) == 0 {
		switch e.Status {
		case "blocked":
			p.Notes = append(p.Notes, fmt.Sprintf("#%d was blocked, so nothing changed.", e.ID))
		case "undone":
			p.Notes = append(p.Notes, fmt.Sprintf("#%d was already undone.", e.ID))
		default:
			p.Notes = append(p.Notes, fmt.Sprintf("#%d has nothing recorded to undo.", e.ID))
		}
		return p, nil
	}
	if e.Status == "undone" {
		p.Notes = append(p.Notes, fmt.Sprintf("#%d was already undone; running it again only restores what changed since.", e.ID))
	}
	accounts := map[string]string{}
	// Buckets this entry copied before deleting them. When such a bucket
	// is gone, its object versions went with it, and the copy brings the
	// objects back instead.
	copied := map[string]bool{}
	for _, s := range e.Undo {
		if s.Kind == "s3-bucket-copy" {
			copied[s.Bucket] = true
		}
	}
	for i := range e.Undo {
		s := &e.Undo[i]
		if err := samePlace(ctx, e.ID, s, clientFor, accounts); err != nil {
			return Plan{}, err
		}
		c := clientFor(s.Conn)
		var err error
		switch s.Kind {
		case "s3-versions":
			err = planVersions(ctx, c, *s, &p, copied[s.Bucket])
			if err != nil && copied[s.Bucket] && awscli.Has(err, "NoSuchBucket") {
				err = nil // the bucket copy below restores these objects
			}
		case "s3-bucket-copy":
			err = planBucket(ctx, c, *s, &p)
		case "ddb-items":
			err = planItems(ctx, c, *s, &p, dataDir, e.ID)
		case "ddb-table-backup":
			err = planTable(ctx, c, s, &p)
		case "rds-snapshot":
			err = planDB(ctx, c, s, &p)
		default:
			err = fmt.Errorf("unknown undo step %q (journal from a newer cloudundo?)", s.Kind)
		}
		if err != nil {
			return Plan{}, err
		}
	}
	return p, nil
}

// copied says this entry also copied the bucket before deleting it.
func planVersions(ctx context.Context, c awscli.Client, s journal.UndoStep, p *Plan, copied bool) error {
	var vs []awscli.Version
	switch {
	case s.Key != "":
		all, err := c.ListVersions(ctx, s.Bucket, s.Key)
		if err != nil {
			return err
		}
		vs = exact(all, s.Key)
	case len(s.Keys) > 0:
		for _, k := range s.Keys {
			all, err := c.ListVersions(ctx, s.Bucket, k)
			if err != nil {
				return err
			}
			vs = append(vs, exact(all, k)...)
		}
	default:
		all, err := c.ListVersions(ctx, s.Bucket, s.Prefix)
		if err != nil {
			return err
		}
		vs = all
	}

	// A bucket that was deleted and then recreated from its copy has no
	// history from before the command: its versions went with it, and the
	// copy already put the objects back as they were.
	if copied {
		history := false
		for _, v := range vs {
			if !v.LastModified.After(s.Since) {
				history = true
				break
			}
		}
		if !history {
			return nil
		}
	}

	byKey := map[string][]awscli.Version{}
	var keys []string
	for _, v := range vs {
		if _, ok := byKey[v.Key]; !ok {
			keys = append(keys, v.Key)
		}
		byKey[v.Key] = append(byKey[v.Key], v)
	}
	sort.Strings(keys)

	for _, key := range keys {
		hist := byKey[key] // newest first
		sort.SliceStable(hist, func(i, j int) bool { return hist[i].LastModified.After(hist[j].LastModified) })

		var after []awscli.Version
		var before *awscli.Version
		for i := range hist {
			if hist[i].LastModified.After(s.Since) {
				after = append(after, hist[i])
			} else {
				before = &hist[i]
				break
			}
		}
		uri := "s3://" + s.Bucket + "/" + key
		if len(after) == 0 {
			continue // untouched
		}
		if before == nil || before.DeleteMarker {
			p.Notes = append(p.Notes, fmt.Sprintf("%s did not exist before, so it is left in place.", uri))
			continue
		}
		if before.VersionID == "" {
			p.Notes = append(p.Notes, fmt.Sprintf("%s has no version history; cannot restore it.", uri))
			continue
		}

		onlyMarkers := true
		for _, a := range after {
			if !a.DeleteMarker {
				onlyMarkers = false
			}
		}
		prev := *before
		if onlyMarkers {
			markers := after
			p.Actions = append(p.Actions, Action{
				Desc: "bring back " + uri,
				do: func(ctx context.Context) error {
					for _, m := range markers {
						err := c.DeleteVersion(ctx, s.Bucket, key, m.VersionID)
						if awscli.Has(err, "AccessDenied") {
							// The `cloudundo policy` forbids removing versions,
							// delete markers included: copy the old version back
							// on top instead, which needs no delete.
							return c.RestoreVersion(ctx, s.Bucket, key, prev.VersionID)
						}
						if err != nil {
							return err
						}
					}
					return nil
				},
			})
			continue
		}
		p.Actions = append(p.Actions, Action{
			Desc: fmt.Sprintf("restore %s to its version from %s", uri, prev.LastModified.UTC().Format("2006-01-02 15:04:05 UTC")),
			do: func(ctx context.Context) error {
				return c.RestoreVersion(ctx, s.Bucket, key, prev.VersionID)
			},
		})
	}
	return nil
}

func planBucket(ctx context.Context, c awscli.Client, s journal.UndoStep, p *Plan) error {
	rc := c
	rc.C.Region = s.Region
	src := "s3://" + s.BackupBucket + "/" + s.BackupPrefix
	dst := "s3://" + s.Bucket

	if ok, err := rc.BucketExists(ctx, s.BackupBucket); err != nil {
		return err
	} else if !ok {
		return fmt.Errorf("the backup bucket s3://%s is gone, so s3://%s cannot be restored", s.BackupBucket, s.Bucket)
	}
	count, _, err := rc.BucketSummary(ctx, s.BackupBucket, s.BackupPrefix)
	if err != nil {
		return err
	}
	if count == 0 {
		return fmt.Errorf("the backup %s is empty, so s3://%s cannot be restored", src, s.Bucket)
	}

	exists, err := rc.BucketExists(ctx, s.Bucket)
	if err != nil {
		return err
	}
	if exists {
		n, _, err := rc.BucketSummary(ctx, s.Bucket, "")
		if err != nil {
			return err
		}
		if n > 0 {
			p.Notes = append(p.Notes, fmt.Sprintf(
				"%s still exists and has %d objects, so it is left as is. The backup stays at %s.", dst, n, src))
			return nil
		}
		p.Actions = append(p.Actions, Action{
			Desc: fmt.Sprintf("copy %d objects from %s back into %s", count, src, dst),
			do:   func(ctx context.Context) error { return rc.Sync(ctx, src, dst) },
		})
		return nil
	}
	p.Notes = append(p.Notes, fmt.Sprintf(
		"%s comes back private, with its objects but not its old settings (policy, website, CORS, tags).", dst))
	p.Actions = append(p.Actions, Action{
		Desc: fmt.Sprintf("recreate %s in %s", dst, s.Region),
		do:   func(ctx context.Context) error { return createOnly(ctx, rc, s.Bucket, s.Region) },
	}, Action{
		Desc: fmt.Sprintf("copy %d objects from %s back into %s", count, src, dst),
		do:   func(ctx context.Context) error { return rc.Sync(ctx, src, dst) },
	})
	return nil
}

// createOnly recreates a bucket. The original's settings (policy, CORS,
// website, tags) are not restored in this version.
func createOnly(ctx context.Context, c awscli.Client, bucket, region string) error {
	err := c.CreateBucket(ctx, bucket, region)
	if awscli.Has(err, "BucketAlreadyOwnedByYou") {
		return nil
	}
	if awscli.Has(err, "BucketAlreadyExists") {
		return fmt.Errorf("someone else now owns the name %s; restore the backup into another bucket by hand", bucket)
	}
	return err
}

func exact(vs []awscli.Version, key string) []awscli.Version {
	var out []awscli.Version
	for _, v := range vs {
		if v.Key == key {
			out = append(out, v)
		}
	}
	return out
}

// Summary is a short human description of a plan.
func (p Plan) Summary() string {
	var b strings.Builder
	for _, a := range p.Actions {
		b.WriteString("  - " + a.Desc + "\n")
	}
	for _, n := range p.Notes {
		b.WriteString("  note: " + n + "\n")
	}
	return b.String()
}

// samePlace makes undo work on the account and region the data was
// protected in. A step whose backup has an ARN takes its region from it
// when the command named none, and undo stops with a clear message when
// the current credentials belong to another account, instead of failing
// later with "not found" or restoring into the wrong place.
func samePlace(ctx context.Context, id int, s *journal.UndoStep, clientFor ClientFor, accounts map[string]string) error {
	account, region := arnPlace(stepARN(s))
	if account == "" {
		return nil
	}
	if s.Conn.Region == "" {
		s.Conn.Region = region
	}
	key := s.Conn.Key()
	current, ok := accounts[key]
	if !ok {
		var err error
		current, err = clientFor(s.Conn).AccountID(ctx)
		if err != nil {
			return fmt.Errorf("cannot check which AWS account the current credentials belong to: %w", err)
		}
		accounts[key] = current
	}
	if current != account {
		return fmt.Errorf("#%d was protected in AWS account %s, but the current credentials are for account %s. "+
			"Use the same AWS profile the agent used, then run undo again", id, account, current)
	}
	return nil
}

func stepARN(s *journal.UndoStep) string {
	switch {
	case s.BackupArn != "":
		return s.BackupArn
	case s.DB != nil:
		return s.DB.Arn
	}
	return ""
}

// arnPlace returns the account and region of an ARN
// (arn:partition:service:region:account:resource).
func arnPlace(arn string) (account, region string) {
	p := strings.SplitN(arn, ":", 6)
	if len(p) < 6 || p[0] != "arn" {
		return "", ""
	}
	return p[4], p[3]
}
