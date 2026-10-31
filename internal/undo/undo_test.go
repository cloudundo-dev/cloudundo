package undo

import (
	"context"
	"strings"
	"testing"

	"github.com/cloudundo-dev/cloudundo/internal/awscli"
	"github.com/cloudundo-dev/cloudundo/internal/awstest"
	"github.com/cloudundo-dev/cloudundo/internal/effects"
	"github.com/cloudundo-dev/cloudundo/internal/journal"
	"github.com/cloudundo-dev/cloudundo/internal/protect"
)

type world struct {
	t  *testing.T
	f  *awstest.Fake
	cf func(awscli.Conn) awscli.Client
}

func newWorld(t *testing.T) *world {
	f := awstest.New()
	return &world{t: t, f: f, cf: func(c awscli.Conn) awscli.Client { return awscli.Client{R: f, C: c} }}
}

// protect runs protection for line and returns the journal entry it would record.
func (w *world) protect(line string) journal.Entry {
	w.t.Helper()
	cfg := protect.Defaults()
	cfg.Now = w.f.Now
	r := protect.Run(context.Background(), effects.Analyze(line), w.cf, cfg)
	if !r.Allowed {
		w.t.Fatalf("%q blocked: %v", line, r.Blocked)
	}
	return journal.Entry{ID: 1, Command: line, Undo: r.Undo, Status: "protected"}
}

func (w *world) undo(e journal.Entry) Plan {
	w.t.Helper()
	p, err := For(context.Background(), &e, w.cf, w.t.TempDir())
	if err != nil {
		w.t.Fatalf("plan: %v", err)
	}
	if _, err := p.Apply(context.Background()); err != nil {
		w.t.Fatalf("apply: %v", err)
	}
	return p
}

func (w *world) want(bucket, key, content string) {
	w.t.Helper()
	got, ok := w.f.Get(bucket, key)
	if !ok || got != content {
		w.t.Fatalf("s3://%s/%s = %q (exists %v), want %q", bucket, key, got, ok, content)
	}
}

func TestUndoDeleteOnUnversionedBucket(t *testing.T) {
	w := newWorld(t)
	w.f.CreateBucket("uploads", "us-east-1", "")
	w.f.Put("uploads", "photo.jpg", "family photo")

	e := w.protect("aws s3 rm s3://uploads/photo.jpg")
	w.f.Delete("uploads", "photo.jpg") // the agent's command runs
	if _, ok := w.f.Get("uploads", "photo.jpg"); ok {
		t.Fatal("delete did not happen")
	}

	p := w.undo(e)
	if len(p.Actions) != 1 || !strings.Contains(p.Actions[0].Desc, "bring back") {
		t.Fatalf("plan: %s", p.Summary())
	}
	w.want("uploads", "photo.jpg", "family photo")
}

func TestUndoOverwrite(t *testing.T) {
	w := newWorld(t)
	w.f.CreateBucket("cfg", "us-east-1", "")
	w.f.Put("cfg", "app.json", `{"db":"prod"}`)

	e := w.protect("aws s3 cp app.json s3://cfg/app.json")
	w.f.Put("cfg", "app.json", `{"db":"oops"}`)

	w.undo(e)
	w.want("cfg", "app.json", `{"db":"prod"}`)
}

func TestUndoRecursiveDeleteLeavesOthersAlone(t *testing.T) {
	w := newWorld(t)
	w.f.CreateBucket("data", "us-east-1", "Enabled")
	w.f.Put("data", "tmp/a", "A")
	w.f.Put("data", "tmp/b", "B")
	w.f.Put("data", "tmp/sub/c", "C")
	w.f.Put("data", "keep/d", "D")

	e := w.protect("aws s3 rm s3://data/tmp/ --recursive")
	w.f.DeletePrefix("data", "tmp/")

	p := w.undo(e)
	if len(p.Actions) != 3 {
		t.Fatalf("want 3 actions:\n%s", p.Summary())
	}
	w.want("data", "tmp/a", "A")
	w.want("data", "tmp/b", "B")
	w.want("data", "tmp/sub/c", "C")
	w.want("data", "keep/d", "D")
}

func TestUndoTheThreeTerabyteCase(t *testing.T) {
	// The Codex incident: a cleanup script deleted almost everything in a bucket.
	w := newWorld(t)
	w.f.CreateBucket("user-files", "us-east-1", "")
	for _, k := range []string{"u1/a.pdf", "u1/b.pdf", "u2/c.png", "u3/d.mp4"} {
		w.f.Put("user-files", k, "data of "+k)
	}
	e := w.protect("aws s3 rm s3://user-files --recursive")
	w.f.DeletePrefix("user-files", "")

	w.undo(e)
	for _, k := range []string{"u1/a.pdf", "u1/b.pdf", "u2/c.png", "u3/d.mp4"} {
		w.want("user-files", k, "data of "+k)
	}
}

func TestUndoDeleteObjectsKeyList(t *testing.T) {
	w := newWorld(t)
	w.f.CreateBucket("b", "us-east-1", "")
	w.f.Put("b", "a.txt", "A")
	w.f.Put("b", "a.txt.bak", "BAK")
	w.f.Put("b", "b.txt", "B")
	e := w.protect("aws s3api delete-objects --bucket b --delete 'Objects=[{Key=a.txt},{Key=b.txt}]'")
	w.f.Delete("b", "a.txt")
	w.f.Delete("b", "b.txt")

	p := w.undo(e)
	if len(p.Actions) != 2 {
		t.Fatalf("plan:\n%s", p.Summary())
	}
	w.want("b", "a.txt", "A")
	w.want("b", "b.txt", "B")
	w.want("b", "a.txt.bak", "BAK")
}

func TestUndoOverwriteThenDelete(t *testing.T) {
	w := newWorld(t)
	w.f.CreateBucket("b", "us-east-1", "")
	w.f.Put("b", "k", "original")
	e := w.protect("aws s3 mv s3://b/k s3://b/k2")
	w.f.Put("b", "k", "changed")
	w.f.Delete("b", "k")

	w.undo(e)
	w.want("b", "k", "original")
}

func TestNewObjectsAreLeftInPlace(t *testing.T) {
	w := newWorld(t)
	w.f.CreateBucket("site", "us-east-1", "")
	e := w.protect("aws s3 cp index.html s3://site/index.html")
	w.f.Put("site", "index.html", "<h1>new</h1>")

	p := w.undo(e)
	if len(p.Actions) != 0 || !strings.Contains(p.Summary(), "did not exist before") {
		t.Fatalf("plan:\n%s", p.Summary())
	}
	// Undo never deletes.
	w.want("site", "index.html", "<h1>new</h1>")
}

func TestUndoTwiceIsHarmless(t *testing.T) {
	w := newWorld(t)
	w.f.CreateBucket("b", "us-east-1", "")
	w.f.Put("b", "k", "v1")
	e := w.protect("aws s3 rm s3://b/k")
	w.f.Delete("b", "k")
	w.undo(e)
	p := w.undo(e)
	if len(p.Actions) != 0 {
		t.Fatalf("second undo should do nothing:\n%s", p.Summary())
	}
	w.want("b", "k", "v1")
}

func TestUndoBucketDelete(t *testing.T) {
	w := newWorld(t)
	w.f.CreateBucket("user-data", "eu-west-1", "")
	w.f.Put("user-data", "a.txt", "alpha")
	w.f.Put("user-data", "dir/b.txt", "bravo")

	e := w.protect("cd infra && aws s3 rb s3://user-data --force")
	w.f.RemoveBucket("user-data")

	p := w.undo(e)
	if len(p.Actions) != 2 || !strings.Contains(p.Summary(), "not its old settings") {
		t.Fatalf("plan:\n%s", p.Summary())
	}
	w.want("user-data", "a.txt", "alpha")
	w.want("user-data", "dir/b.txt", "bravo")
}

func TestUndoBucketDeleteThatDidNotHappen(t *testing.T) {
	w := newWorld(t)
	w.f.CreateBucket("b", "us-east-1", "")
	w.f.Put("b", "k", "v")
	e := w.protect("aws s3 rb s3://b --force")
	// The delete failed; the bucket and its data are still there.
	p := w.undo(e)
	if len(p.Actions) != 0 || !strings.Contains(p.Summary(), "left as is") {
		t.Fatalf("plan:\n%s", p.Summary())
	}
}

func TestUndoBucketDeleteAfterObjectsWereRemoved(t *testing.T) {
	// rb --force empties the bucket first; if the final delete failed,
	// the bucket is left empty.
	w := newWorld(t)
	w.f.CreateBucket("b", "us-east-1", "")
	w.f.Put("b", "k", "v")
	e := w.protect("aws s3 rb s3://b --force")
	w.f.DeletePrefix("b", "")
	w.undo(e)
	w.want("b", "k", "v")
}

func TestMissingBackupIsAnError(t *testing.T) {
	w := newWorld(t)
	w.f.CreateBucket("b", "us-east-1", "")
	w.f.Put("b", "k", "v")
	e := w.protect("aws s3 rb s3://b --force")
	w.f.RemoveBucket("b")
	w.f.RemoveBucket(e.Undo[0].BackupBucket)
	if _, err := For(context.Background(), &e, w.cf, w.t.TempDir()); err == nil || !strings.Contains(err.Error(), "is gone") {
		t.Fatalf("err = %v", err)
	}
}

func TestNothingToUndo(t *testing.T) {
	w := newWorld(t)
	blocked := journal.Entry{ID: 4, Status: "blocked"}
	p, err := For(context.Background(), &blocked, w.cf, "")
	if err != nil || !strings.Contains(p.Summary(), "was blocked") {
		t.Fatalf("%v %s", err, p.Summary())
	}
}

func TestApplyStopsAtFirstFailure(t *testing.T) {
	w := newWorld(t)
	w.f.CreateBucket("b", "us-east-1", "")
	w.f.Put("b", "x", "X")
	w.f.Put("b", "y", "Y")
	e := w.protect("aws s3 rm s3://b --recursive")
	w.f.DeletePrefix("b", "")
	p, err := For(context.Background(), &e, w.cf, w.t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	w.f.Fail["delete-object --bucket b --key x"] = "An error occurred (InternalError)"
	n, err := p.Apply(context.Background())
	if err == nil || n != 0 || !strings.Contains(err.Error(), "s3://b/x") {
		t.Fatalf("n=%d err=%v", n, err)
	}
}

func TestUndoWorksWhenRemovingVersionsIsDenied(t *testing.T) {
	w := newWorld(t)
	w.f.CreateBucket("b", "us-east-1", "")
	w.f.Put("b", "x", "X")
	e := w.protect("aws s3 rm s3://b/x")
	w.f.Delete("b", "x")
	// What `cloudundo policy` does: no version may be removed.
	w.f.Fail["--version-id"] = "An error occurred (AccessDenied) when calling the DeleteObject operation"
	p := w.undo(e)
	if len(p.Actions) != 1 {
		t.Fatalf("plan = %+v", p)
	}
	w.want("b", "x", "X")
}

// "Empty the bucket, then delete it" is a common agent command. Turning
// versioning on would make the delete fail (a versioned bucket is never
// empty), so the bucket is only copied, and undo brings it back from the
// copy.
func TestEmptyThenDeleteBucket(t *testing.T) {
	w := newWorld(t)
	w.f.CreateBucket("tmp-data", "us-east-1", "")
	w.f.Put("tmp-data", "a", "A")
	w.f.Put("tmp-data", "b/c", "C")
	e := w.protect("aws s3 rm s3://tmp-data --recursive && aws s3 rb s3://tmp-data")
	if w.f.Versioning("tmp-data") != "" {
		t.Fatal("versioning turned on: the rb in the same command would fail")
	}
	if len(e.Undo) != 1 || e.Undo[0].Kind != "s3-bucket-copy" {
		t.Fatalf("undo steps: %+v", e.Undo)
	}
	w.f.RemoveBucket("tmp-data")
	w.apply(w.undo(e))
	if v, ok := w.f.Get("tmp-data", "b/c"); !ok || v != "C" {
		t.Fatalf("b/c = %q %v", v, ok)
	}
}
