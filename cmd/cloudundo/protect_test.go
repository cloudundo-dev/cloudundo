package main

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/cloudundo-dev/cloudundo/internal/awscli"
	"github.com/cloudundo-dev/cloudundo/internal/awstest"
	"github.com/cloudundo-dev/cloudundo/internal/journal"
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
	if code, out, errOut := runCLI(t, "undo", "1", "--yes"); code != 0 || !strings.Contains(out, "Restored (1 step)") {
		t.Fatalf("undo: %d\n%s%s", code, out, errOut)
	}
	if got := f.Item("Users", `{"pk":{"S":"u1"}}`); !strings.Contains(got, `"N":"1"`) {
		t.Fatalf("item = %s", got)
	}
}

func TestPolicyCommand(t *testing.T) {
	code, out, errOut := runCLI(t, "policy", "--account", "123456789012", "--region", "eu-west-1")
	if code != 0 || !strings.HasPrefix(out, "{") || !strings.Contains(out, "cloudundo-backup-123456789012-eu-west-1") {
		t.Fatalf("exit %d\n%s%s", code, out, errOut)
	}
	if strings.Contains(out, "aws iam") || !strings.Contains(errOut, "put-user-policy") || !strings.Contains(errOut, "LocationConstraint=eu-west-1") {
		t.Fatalf("instructions must go to stderr only:\n%s", errOut)
	}
	if code, _, _ := runCLI(t, "policy", "--account", "12"); code != 2 {
		t.Fatalf("bad account accepted: %d", code)
	}
	if code, out, _ := runCLI(t, "policy"); code != 0 || !strings.Contains(out, "cloudundo-backup-*") {
		t.Fatalf("default: %d %s", code, out)
	}
}

func TestProtectTerraformDestroyThenUndo(t *testing.T) {
	f := withFake(t)
	f.CreateDB(awscli.DBInfo{ID: "courses-db", Engine: "postgres", Class: "db.t4g.micro"}, "answers")
	bin := t.TempDir()
	plan := `{"resource_changes":[{"address":"aws_db_instance.main","mode":"managed","type":"aws_db_instance","change":{"actions":["delete"],"before":{"identifier":"courses-db","skip_final_snapshot":true,"arn":"arn:aws:rds:us-east-1:123456789012:db:courses-db"}}}]}`
	script := "#!/bin/sh\ncase \"$1\" in show) echo '" + plan + "';; plan) exit 0;; *) echo \"refusing $1\" >&2; exit 9;; esac\n"
	if err := os.WriteFile(filepath.Join(bin, "terraform"), []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", bin+string(os.PathListSeparator)+os.Getenv("PATH"))
	infra := filepath.Join(t.TempDir(), "infra")
	os.Mkdir(infra, 0o755)
	t.Chdir(filepath.Dir(infra))

	code, out, errOut := runCLI(t, "protect", "--", "cd infra && terraform destroy -auto-approve")
	if code != 0 || !strings.Contains(out, "made snapshot cloudundo-courses-db-") {
		t.Fatalf("protect: %d\n%s%s", code, out, errOut)
	}
	f.DropDB("courses-db", false, "")
	runCLI(t, "undo", "1", "--yes")
	code, out, errOut = runCLI(t, "undo", "1", "--yes")
	if code != 0 || !strings.Contains(out, "Restored") {
		t.Fatalf("undo: %d\n%s%s", code, out, errOut)
	}
	if _, data, ok := f.DB("courses-db", false); !ok || data != "answers" {
		t.Fatalf("db = %v %q", ok, data)
	}
}

// The agent's profile and region come from its environment, not the
// command line. They are kept with the entry, so undo from a differently
// set-up shell still talks to the same account and region; what the
// command line sets itself wins, and credentials are never kept.
func TestProtectKeepsTheAgentsProfileAndRegion(t *testing.T) {
	f := withFake(t)
	f.CreateBucket("uploads", "us-west-1", "")
	f.Put("uploads", "a", "A")
	agentEnv := map[string]string{
		"AWS_PROFILE": "agent", "AWS_REGION": "us-west-1",
		"AWS_SECRET_ACCESS_KEY": "do-not-keep", "HOME": "/home/x",
	}
	old := getenv
	getenv = func(k string) string { return agentEnv[k] }
	t.Cleanup(func() { getenv = old })

	if code, out, errOut := runCLI(t, "protect", "--", "AWS_REGION=eu-west-1 aws s3 rm s3://uploads/a"); code != 0 {
		t.Fatalf("exit %d\n%s%s", code, out, errOut)
	}
	es := journalEntries(t)
	if len(es) != 1 || len(es[0].Effects) == 0 {
		t.Fatalf("entries = %+v", es)
	}
	env := es[0].Effects[0].Conn.Env
	if env["AWS_PROFILE"] != "agent" || env["AWS_REGION"] != "eu-west-1" {
		t.Fatalf("kept env = %v; want the agent's profile and the command's own region", env)
	}
	for _, step := range es[0].Undo {
		if step.Conn.Env["AWS_PROFILE"] != "agent" {
			t.Fatalf("undo step conn = %+v; want the agent's profile", step.Conn)
		}
	}
	if b, _ := os.ReadFile(journalFile(t)); strings.Contains(string(b), "do-not-keep") || strings.Contains(string(b), "/home/x") {
		t.Fatal("the journal kept a credential or an unrelated variable")
	}
}

func journalFile(t *testing.T) string {
	t.Helper()
	dir, err := journal.DefaultDir()
	if err != nil {
		t.Fatal(err)
	}
	return filepath.Join(dir, "journal.jsonl")
}

// slowRunner makes every AWS call take a while, and gives up when the
// caller's time is up, like the real aws CLI under a context.
type slowRunner struct {
	awscli.Runner
	delay time.Duration
}

func (s slowRunner) Run(ctx context.Context, env []string, args ...string) ([]byte, error) {
	select {
	case <-time.After(s.delay):
	case <-ctx.Done():
		return nil, ctx.Err()
	}
	return s.Runner.Run(ctx, env, args...)
}

// Protection that takes longer than the agent allows ends in a block that
// cloudundo gives itself, before the agent kills it.
func TestProtectStopsBeforeTheAgentsTimeout(t *testing.T) {
	f := withFake(t)
	f.CreateBucket("uploads", "us-east-1", "")
	f.Put("uploads", "a", "A")
	newRunner = func() awscli.Runner { return slowRunner{f, 30 * time.Millisecond} }
	old := protectBudget
	protectBudget = 50 * time.Millisecond
	t.Cleanup(func() { protectBudget = old })

	code, out, errOut := runCLI(t, "protect", "--", "aws s3 rb s3://uploads --force")
	if code != exitBlock || !strings.Contains(out+errOut, "ran out of time") {
		t.Fatalf("exit %d\n%s%s", code, out, errOut)
	}
}
