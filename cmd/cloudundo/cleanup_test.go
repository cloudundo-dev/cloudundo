package main

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/cloudundo/cloudundo/internal/awscli"
)

func TestCleanupListsThenDeletesOnlyWhatWasChosen(t *testing.T) {
	f := withFake(t)
	f.CreateBucket("old-files", "us-east-1", "")
	f.Put("old-files", "a", "A")
	f.CreateTable("old-users", "pk")
	f.PutItem("old-users", `{"pk":{"S":"u1"}}`)
	f.CreateDB(awscli.DBInfo{ID: "old-db", Engine: "postgres", Class: "db.t3.micro"}, "rows")
	f.CreateTable("live", "pk")
	f.PutItem("live", `{"pk":{"S":"k"}}`)

	for _, line := range []string{
		"aws s3 rb s3://old-files --force",
		"aws dynamodb delete-table --table-name old-users",
		"aws rds delete-db-instance --db-instance-identifier old-db --skip-final-snapshot",
	} {
		if code, out, errOut := runCLI(t, "protect", "--", line); code != 0 {
			t.Fatalf("%s: %d\n%s%s", line, code, out, errOut)
		}
	}
	es := journalEntries(t)
	copyStep, backupStep, snapStep := es[0].Undo[0], es[1].Undo[0], es[2].Undo[0]

	f.SetClock(f.Now().Add(40 * 24 * time.Hour)) // 40 days later
	if code, out, _ := runCLI(t, "protect", "--", `aws dynamodb delete-item --table-name live --key '{"pk":{"S":"k"}}'`); code != 0 {
		t.Fatalf("recent protect: %s", out)
	}

	// Listing changes nothing.
	code, out, _ := runCLI(t, "cleanup")
	if code != 0 || !strings.Contains(out, "4 backup(s). Nothing deleted.") || !strings.Contains(out, "RDS snapshot") {
		t.Fatalf("list: %d\n%s", code, out)
	}
	if !f.HasDBSnapshot(snapStep.Snapshot) || !f.HasBackup(backupStep.BackupArn) {
		t.Fatal("listing deleted something")
	}

	// --yes needs a choice.
	if code, _, errOut := runCLI(t, "cleanup", "--yes"); code != 2 || !strings.Contains(errOut, "say which") {
		t.Fatalf("cleanup --yes alone: %d %s", code, errOut)
	}

	// Older than 30 days: the three old ones, not the recent item save.
	code, out, errOut := runCLI(t, "cleanup", "--older-than", "30d", "--yes")
	if code != 0 || strings.Count(out, "deleted ") != 3 {
		t.Fatalf("cleanup: %d\n%s%s", code, out, errOut)
	}
	if f.HasDBSnapshot(snapStep.Snapshot) || f.HasBackup(backupStep.BackupArn) {
		t.Fatal("snapshot or backup still there")
	}
	if n := f.Count(copyStep.BackupBucket, copyStep.BackupPrefix); n != 0 {
		t.Fatalf("%d versions of the bucket copy left", n)
	}
	if _, out, _ := runCLI(t, "cleanup"); !strings.Contains(out, "1 backup(s)") {
		t.Fatalf("the recent backup must stay:\n%s", out)
	}

	// Undo now says plainly what it cannot bring back.
	f.DropTable("old-users")
	code, out, _ = runCLI(t, "undo", "2")
	if code != 0 || !strings.Contains(out, "deleted by cloudundo cleanup") {
		t.Fatalf("undo after cleanup: %d\n%s", code, out)
	}

	// By entry number, flags after it.
	code, out, _ = runCLI(t, "cleanup", "4", "--yes")
	if code != 0 || !strings.Contains(out, "#4: deleted saved items") {
		t.Fatalf("cleanup 4 --yes: %d\n%s", code, out)
	}
	if _, out, _ := runCLI(t, "cleanup"); !strings.Contains(out, "No backups to clean up") {
		t.Fatalf("after all:\n%s", out)
	}
}

func TestCleanupKeepsGoingAndReportsDeniedDeletes(t *testing.T) {
	f := withFake(t)
	f.CreateTable("t1", "pk")
	f.CreateTable("t2", "pk")
	runCLI(t, "protect", "--", "aws dynamodb delete-table --table-name t1")
	runCLI(t, "protect", "--", "aws dynamodb delete-table --table-name t2")
	b1 := journalEntries(t)[0].Undo[0].BackupArn
	f.Fail[b1] = "An error occurred (AccessDeniedException): explicit deny in an identity-based policy"

	code, out, errOut := runCLI(t, "cleanup", "--all", "--yes")
	if code != 1 || !strings.Contains(out, "#2: deleted") || !strings.Contains(errOut, "AccessDenied") || !strings.Contains(errOut, "cloudundo policy") {
		t.Fatalf("exit %d\n%s%s", code, out, errOut)
	}
	if _, out, _ := runCLI(t, "cleanup"); !strings.Contains(out, "1 backup(s)") || !strings.Contains(out, "#1") {
		t.Fatalf("the denied backup must still be listed:\n%s", out)
	}
}

func TestAgentsCannotRunCleanup(t *testing.T) {
	withFake(t)
	for _, agent := range []string{agentClaude, agentCodex, agentCursor} {
		for _, line := range []string{"cloudundo cleanup --all --yes", "cd /tmp && ~/.local/bin/cloudundo cleanup 3 --yes"} {
			in := claudeInput(line)
			if agent == agentCursor {
				in = cursorInput(line)
			}
			if code, _, errOut := hookRun(t, agent, in); code != exitBlock || !strings.Contains(errOut, "Only the user runs it") {
				t.Fatalf("%s %q: exit %d %s", agent, line, code, errOut)
			}
		}
		in := claudeInput("cloudundo cleanup")
		if agent == agentCursor {
			in = cursorInput("cloudundo cleanup")
		}
		if code, _, _ := hookRun(t, agent, in); code != 0 {
			t.Fatalf("%s: listing backups is harmless, got exit %d", agent, code)
		}
	}
}

func TestCleanupSaysWhenABackupWasAlreadyGone(t *testing.T) {
	f := withFake(t)
	f.CreateTable("t1", "pk")
	runCLI(t, "protect", "--", "aws dynamodb delete-table --table-name t1")
	arn := journalEntries(t)[0].Undo[0].BackupArn
	runCLI(t, "cleanup", "1", "--yes")
	if f.HasBackup(arn) {
		t.Fatal("not deleted")
	}

	f.CreateTable("t2", "pk")
	runCLI(t, "protect", "--", "aws dynamodb delete-table --table-name t2")
	arn2 := journalEntries(t)[1].Undo[0].BackupArn
	// Someone deletes the backup by hand first.
	if _, err := (awscli.Client{R: f}).DeleteBackup(context.Background(), arn2); err != nil {
		t.Fatal(err)
	}
	code, out, _ := runCLI(t, "cleanup", "2", "--yes")
	if code != 0 || !strings.Contains(out, "#2: already gone") {
		t.Fatalf("exit %d\n%s", code, out)
	}
}
