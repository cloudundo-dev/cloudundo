package main

import (
	"strings"
	"testing"

	"github.com/cloudundo/cloudundo/internal/awscli"
	"github.com/cloudundo/cloudundo/internal/awstest"
)

func withFake(t *testing.T) *awstest.Fake {
	t.Helper()
	t.Setenv("CLOUDUNDO_HOME", t.TempDir())
	f := awstest.New()
	oldRunner, oldNow := newRunner, now
	newRunner = func() awscli.Runner { return f }
	now = f.Now
	t.Cleanup(func() { newRunner, now = oldRunner, oldNow })
	return f
}

func TestProtectThenUndo(t *testing.T) {
	f := withFake(t)
	f.CreateBucket("uploads", "us-east-1", "")
	f.Put("uploads", "u1/a.pdf", "A")
	f.Put("uploads", "u2/b.pdf", "B")

	code, out, errOut := runCLI(t, "protect", "--", "aws s3 rm s3://uploads --recursive")
	if code != 0 {
		t.Fatalf("protect exit %d\n%s%s", code, out, errOut)
	}
	if !strings.Contains(out, "protected #1") || !strings.Contains(out, "turned on versioning") || !strings.Contains(out, "cloudundo undo 1") {
		t.Fatalf("protect output:\n%s", out)
	}

	f.DeletePrefix("uploads", "") // the agent's command runs

	// Preview first: nothing changes.
	code, out, _ = runCLI(t, "undo", "1")
	if code != 0 || !strings.Contains(out, "Nothing changed yet") {
		t.Fatalf("preview: %d\n%s", code, out)
	}
	if _, ok := f.Get("uploads", "u1/a.pdf"); ok {
		t.Fatal("preview restored data")
	}

	code, out, errOut = runCLI(t, "undo", "1", "--yes")
	if code != 0 || !strings.Contains(out, "Restored (2 steps)") {
		t.Fatalf("undo: %d\n%s%s", code, out, errOut)
	}
	for k, want := range map[string]string{"u1/a.pdf": "A", "u2/b.pdf": "B"} {
		if got, ok := f.Get("uploads", k); !ok || got != want {
			t.Fatalf("%s = %q, %v", k, got, ok)
		}
	}

	code, out, _ = runCLI(t, "log")
	if code != 0 || !strings.Contains(out, "undone") {
		t.Fatalf("log:\n%s", out)
	}
}

func TestProtectBlocks(t *testing.T) {
	f := withFake(t)
	code, _, errOut := runCLI(t, "protect", "--", "aws s3 rm s3://$BUCKET --recursive")
	if code != exitBlock || !strings.Contains(errOut, "blocked #1") {
		t.Fatalf("exit %d\n%s", code, errOut)
	}
	if len(f.Calls) != 0 {
		t.Fatalf("AWS called: %v", f.Calls)
	}
	_, out, _ := runCLI(t, "log")
	if !strings.Contains(out, "blocked") {
		t.Fatalf("log:\n%s", out)
	}
}

func TestProtectIgnoresOtherCommands(t *testing.T) {
	f := withFake(t)
	code, out, errOut := runCLI(t, "protect", "--", "npm test && aws s3 ls")
	if code != 0 || out != "" || errOut != "" {
		t.Fatalf("exit %d out=%q err=%q", code, out, errOut)
	}
	if len(f.Calls) != 0 {
		t.Fatal("AWS called for a read-only command")
	}
}

func TestProtectFailsClosedOnBadSettings(t *testing.T) {
	f := withFake(t)
	f.CreateBucket("b", "us-east-1", "")
	t.Setenv("CLOUDUNDO_RETENTION_DAYS", "seven")
	code, _, errOut := runCLI(t, "protect", "--", "aws s3 rm s3://b/k")
	if code != exitBlock || !strings.Contains(errOut, "CLOUDUNDO_RETENTION_DAYS") {
		t.Fatalf("exit %d\n%s", code, errOut)
	}
}

func TestProtectFailsClosedWithoutJournal(t *testing.T) {
	f := withFake(t)
	f.CreateBucket("b", "us-east-1", "")
	t.Setenv("CLOUDUNDO_HOME", "/proc/cannot-write-here")
	if code, _, _ := runCLI(t, "protect", "--", "aws s3 rm s3://b/k"); code != exitBlock {
		t.Fatalf("exit %d, want %d", code, exitBlock)
	}
}

func TestRetentionSetting(t *testing.T) {
	f := withFake(t)
	f.CreateBucket("b", "us-east-1", "")
	t.Setenv("CLOUDUNDO_RETENTION_DAYS", "30")
	if code, _, errOut := runCLI(t, "protect", "--", "aws s3 rm s3://b/k"); code != 0 {
		t.Fatalf("exit %d %s", code, errOut)
	}
	if !strings.Contains(f.Lifecycle("b"), `"NoncurrentDays":30`) {
		t.Fatalf("lifecycle = %s", f.Lifecycle("b"))
	}
}

func TestProtectThenUndoDynamoDBTable(t *testing.T) {
	f := withFake(t)
	f.CreateTable("Users", "pk")
	f.SetTableSettings("Users", "expires", map[string]string{"team": "core"}, true, "")
	f.PutItem("Users", `{"pk":{"S":"u1"}}`)

	code, out, errOut := runCLI(t, "check", "--", "aws dynamodb delete-table --table-name Users")
	if code != 0 || !strings.Contains(out, "DELETE TABLE dynamodb:Users") {
		t.Fatalf("check: %d\n%s%s", code, out, errOut)
	}
	code, out, errOut = runCLI(t, "protect", "--", "aws dynamodb delete-table --table-name Users")
	if code != 0 || !strings.Contains(out, "on-demand backup") {
		t.Fatalf("protect: %d\n%s%s", code, out, errOut)
	}
	f.DropTable("Users")

	code, out, errOut = runCLI(t, "undo", "1", "--yes")
	if code != 0 || !strings.Contains(out, "again in a few minutes") || !f.HasTable("Users") {
		t.Fatalf("undo run 1: %d\n%s%s", code, out, errOut)
	}
	if _, out, _ := runCLI(t, "log"); strings.Contains(out, "undone") {
		t.Fatalf("marked undone before settings came back:\n%s", out)
	}

	code, out, errOut = runCLI(t, "undo", "1", "--yes")
	if code != 0 || !strings.Contains(out, "Restored") {
		t.Fatalf("undo run 2: %d\n%s%s", code, out, errOut)
	}
	if ttl, tags, pitr, _ := f.TableSettings("Users"); ttl != "expires" || tags["team"] != "core" || !pitr {
		t.Fatalf("settings not back: %q %v %v", ttl, tags, pitr)
	}
	if f.Item("Users", `{"pk":{"S":"u1"}}`) == "" {
		t.Fatal("item not back")
	}
	if _, out, _ := runCLI(t, "log"); !strings.Contains(out, "undone") {
		t.Fatalf("log:\n%s", out)
	}
	if code, out, _ := runCLI(t, "undo", "1", "--yes"); code != 0 || !strings.Contains(out, "Nothing to restore") {
		t.Fatalf("undo run 3: %d\n%s", code, out)
	}
}

func TestProtectThenUndoDynamoDBItem(t *testing.T) {
	f := withFake(t)
	f.CreateTable("Users", "pk")
	f.PutItem("Users", `{"pk":{"S":"u1"},"n":{"N":"1"}}`)
	line := `aws dynamodb delete-item --table-name Users --key '{"pk":{"S":"u1"}}'`
	if code, out, errOut := runCLI(t, "protect", "--", line); code != 0 {
		t.Fatalf("protect: %d\n%s%s", code, out, errOut)
	}
	f.DeleteItem("Users", `{"pk":{"S":"u1"}}`)
	if code, out, errOut := runCLI(t, "undo", "1", "--yes"); code != 0 || !strings.Contains(out, "Restored (1 steps)") {
		t.Fatalf("undo: %d\n%s%s", code, out, errOut)
	}
	if got := f.Item("Users", `{"pk":{"S":"u1"}}`); !strings.Contains(got, `"N":"1"`) {
		t.Fatalf("item = %s", got)
	}
}
