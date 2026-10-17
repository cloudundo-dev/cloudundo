package awscli

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

// fakeBin writes a shell script standing in for aws.
func fakeBin(t *testing.T, script string) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), "aws")
	if err := os.WriteFile(p, []byte("#!/bin/sh\n"+script), 0o755); err != nil {
		t.Fatal(err)
	}
	return p
}

func TestExecPassesArgsAndEnv(t *testing.T) {
	log := filepath.Join(t.TempDir(), "log")
	bin := fakeBin(t, `echo "$@" > `+log+`; echo "profile=$AWS_PROFILE key=$AWS_SECRET_ACCESS_KEY pager=[$AWS_PAGER]" >> `+log+`; echo '{"Status":"Enabled"}'`)
	c := Client{R: Exec{Bin: bin}, C: ConnFromEnv(map[string]string{"AWS_PROFILE": "prod", "AWS_SECRET_ACCESS_KEY": "s"})}
	c.C.Region = "eu-west-1"

	st, err := c.VersioningStatus(context.Background(), "b")
	if err != nil || st != "Enabled" {
		t.Fatalf("status %q, err %v", st, err)
	}
	got, _ := os.ReadFile(log)
	want := "s3api get-bucket-versioning --bucket b --region eu-west-1 --output json\nprofile=prod key=s pager=[]\n"
	if string(got) != want {
		t.Fatalf("got\n%s\nwant\n%s", got, want)
	}
}

func TestExecErrors(t *testing.T) {
	bin := fakeBin(t, `echo "An error occurred (NoSuchLifecycleConfiguration) when calling" >&2; exit 254`)
	c := Client{R: Exec{Bin: bin}}
	_, ok, err := c.GetLifecycle(context.Background(), "b")
	if err != nil || ok {
		t.Fatalf("missing lifecycle should be ok=false, err=nil; got %v, %v", ok, err)
	}

	bin = fakeBin(t, `echo "An error occurred (AccessDenied)" >&2; exit 254`)
	c = Client{R: Exec{Bin: bin}}
	_, err = c.VersioningStatus(context.Background(), "b")
	if err == nil || !Has(err, "AccessDenied") || !strings.Contains(err.Error(), "get-bucket-versioning") {
		t.Fatalf("err = %v", err)
	}

	c = Client{R: Exec{Bin: filepath.Join(t.TempDir(), "missing")}}
	if _, err := c.VersioningStatus(context.Background(), "b"); err == nil {
		t.Fatal("missing aws binary should fail")
	}
}

func TestSecretsNotSerialized(t *testing.T) {
	c := ConnFromEnv(map[string]string{"AWS_SECRET_ACCESS_KEY": "s", "AWS_PROFILE": "p"})
	b, _ := jsonMarshal(c)
	if strings.Contains(string(b), "AWS_SECRET") {
		t.Fatalf("secret leaked: %s", b)
	}
}

func TestExecGivesUpOnAHungCall(t *testing.T) {
	bin := fakeBin(t, "exec sleep 30\n")
	start := time.Now()
	_, err := Exec{Bin: bin, Timeout: 300 * time.Millisecond}.Run(context.Background(), nil, "s3api", "head-bucket")
	if err == nil || !strings.Contains(err.Error(), "did not answer") {
		t.Fatalf("err = %v", err)
	}
	if d := time.Since(start); d > 10*time.Second {
		t.Fatalf("took %s", d)
	}
}

func TestExecGivesSyncMoreTime(t *testing.T) {
	bin := fakeBin(t, "sleep 0.5; echo ok\n")
	out, err := Exec{Bin: bin, Timeout: 100 * time.Millisecond, SyncTimeout: 5 * time.Second}.Run(context.Background(), nil, "s3", "sync", "a", "b")
	if err != nil || strings.TrimSpace(string(out)) != "ok" {
		t.Fatalf("out %q err %v", out, err)
	}
}

func TestExecReportsSlowCalls(t *testing.T) {
	old := slowAfter
	slowAfter = 50 * time.Millisecond
	t.Cleanup(func() { slowAfter = old })
	bin := fakeBin(t, "sleep 0.4\n")
	var buf syncBuffer
	if _, err := (Exec{Bin: bin, Progress: &buf}).Run(context.Background(), nil, "s3", "sync", "s3://a", "s3://b"); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(buf.String(), "waiting for AWS: aws s3 sync s3://a s3://b") {
		t.Fatalf("progress = %q", buf.String())
	}

	buf = syncBuffer{}
	fast := fakeBin(t, "true\n")
	if _, err := (Exec{Bin: fast, Progress: &buf}).Run(context.Background(), nil, "sts", "get-caller-identity"); err != nil {
		t.Fatal(err)
	}
	time.Sleep(100 * time.Millisecond)
	if buf.String() != "" {
		t.Fatalf("fast call reported: %q", buf.String())
	}
}

type syncBuffer struct {
	mu sync.Mutex
	b  bytes.Buffer
}

func (s *syncBuffer) Write(p []byte) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.Write(p)
}

func (s *syncBuffer) String() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.String()
}

func TestExecGivesUpEvenIfAChildKeepsRunning(t *testing.T) {
	if testing.Short() {
		t.Skip("waits for WaitDelay")
	}
	bin := fakeBin(t, "sleep 30\n") // sh forks sleep, which keeps the pipes open
	start := time.Now()
	_, err := Exec{Bin: bin, Timeout: 200 * time.Millisecond}.Run(context.Background(), nil, "sts", "get-caller-identity")
	if err == nil {
		t.Fatal("no error")
	}
	if d := time.Since(start); d > 15*time.Second {
		t.Fatalf("took %s", d)
	}
}
