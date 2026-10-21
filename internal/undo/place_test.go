package undo

import (
	"context"
	"strings"
	"testing"
)

// Undo run from a shell with other credentials must not restore into, or
// look for backups in, another account.
func TestUndoStopsInAnotherAccount(t *testing.T) {
	w, dir := ddbWorld(t)
	e := w.protectDDB(dir, "aws dynamodb delete-table --table-name Users")
	w.f.DropTable("Users")
	w.f.Account = "999999999999" // the user's shell now uses another account

	_, err := For(context.Background(), &e, w.cf, dir)
	if err == nil || !strings.Contains(err.Error(), "was protected in AWS account 123456789012") ||
		!strings.Contains(err.Error(), "999999999999") {
		t.Fatalf("err = %v; want the two accounts named", err)
	}
	for _, c := range w.f.Calls {
		if strings.Contains(strings.Join(c, " "), "restore-table") {
			t.Fatalf("restored in the wrong account: %v", c)
		}
	}
}

// A command that named no region ran in the agent's default region; undo
// takes that region from the backup's ARN instead of the shell's default.
func TestUndoUsesTheBackupsRegion(t *testing.T) {
	w, dir := ddbWorld(t)
	e := w.protectDDB(dir, "aws dynamodb delete-table --table-name Users")
	w.f.DropTable("Users")
	e.Undo[0].Conn.Region = ""
	arn := e.Undo[0].BackupArn
	e.Undo[0].BackupArn = strings.Replace(arn, ":us-east-1:", ":us-west-1:", 1)
	if e.Undo[0].BackupArn == arn {
		t.Fatalf("fake backup ARN %q has no us-east-1 to replace", arn)
	}

	p := w.plan(&e, dir)
	if e.Undo[0].Conn.Region != "us-west-1" {
		t.Fatalf("region = %q, want us-west-1 from the ARN", e.Undo[0].Conn.Region)
	}
	p.Apply(context.Background()) // the fake has no us-west-1 backup; only the call matters
	found := false
	for _, c := range w.f.Calls {
		j := strings.Join(c, " ")
		if strings.Contains(j, "restore-table-from-backup") {
			found = true
			if !strings.Contains(j, "--region us-west-1") {
				t.Fatalf("restore call without the backup's region: %s", j)
			}
		}
	}
	if !found {
		t.Fatal("no restore call")
	}
}

func TestArnPlace(t *testing.T) {
	for _, c := range []struct{ arn, account, region string }{
		{"arn:aws:rds:us-west-1:111122223333:db:main", "111122223333", "us-west-1"},
		{"arn:aws:dynamodb:eu-central-1:111122223333:table/T/backup/01", "111122223333", "eu-central-1"},
		{"", "", ""},
		{"not-an-arn", "", ""},
		{"arn:aws:s3:::bucket", "", ""},
	} {
		a, r := arnPlace(c.arn)
		if a != c.account || r != c.region {
			t.Errorf("arnPlace(%q) = %q, %q", c.arn, a, r)
		}
	}
}
