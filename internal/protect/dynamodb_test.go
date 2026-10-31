package protect

import (
	"bytes"
	"encoding/json"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/cloudundo-dev/cloudundo/internal/journal"
)

func ddbSetup(t *testing.T) (*Config, func(string) Result) {
	t.Helper()
	f, cf, cfg := setup(t)
	cfg.DataDir = t.TempDir()
	cfg.Sleep = func(time.Duration) {}
	f.CreateTable("Users", "pk")
	f.SetTableSettings("Users", "expires", map[string]string{"team": "core"}, true, "NEW_AND_OLD_IMAGES")
	f.PutItem("Users", `{"pk":{"S":"u1"},"name":{"S":"Ali"}}`)
	ddbFake = f
	return &cfg, func(line string) Result { return protect(t, f, cf, cfg, line) }
}

var ddbFake interface {
	Item(string, string) string
}

func savedItems(t *testing.T, file string) []journal.SavedItem {
	t.Helper()
	b, err := os.ReadFile(file)
	if err != nil {
		t.Fatal(err)
	}
	var s []journal.SavedItem
	if err := json.Unmarshal(b, &s); err != nil {
		t.Fatal(err)
	}
	return s
}

func TestTableDeleteIsBackedUpWithSettings(t *testing.T) {
	_, run := ddbSetup(t)
	r := run("aws dynamodb delete-table --table-name Users")
	if !r.Allowed {
		t.Fatalf("blocked: %v", r.Blocked)
	}
	if len(r.Undo) != 1 || r.Undo[0].Kind != "ddb-table-backup" || r.Undo[0].BackupArn == "" {
		t.Fatalf("undo = %+v", r.Undo)
	}
	tc := r.Undo[0].TableConfig
	if tc.TTLAttribute != "expires" || !tc.PITR || tc.StreamViewType != "NEW_AND_OLD_IMAGES" || len(tc.Tags) != 1 {
		t.Fatalf("settings = %+v", tc)
	}
}

func TestTableDeleteWaitsForTheBackup(t *testing.T) {
	f, cf, cfg := setup(t)
	cfg.Sleep = func(time.Duration) {}
	f.CreateTable("T", "id")
	f.BackupPolls = 3
	if r := protect(t, f, cf, cfg, "aws dynamodb delete-table --table-name T"); !r.Allowed {
		t.Fatalf("blocked: %v", r.Blocked)
	}
	if n := calls(f, "describe-backup"); n != 4 {
		t.Fatalf("describe-backup called %d times, want 4", n)
	}

	// A backup that never becomes ready blocks the delete.
	f, cf, cfg = setup(t)
	cfg.Sleep = func(time.Duration) {}
	cfg.BackupWait = 5 * time.Second
	f.CreateTable("T", "id")
	f.BackupPolls = 1000
	r := protect(t, f, cf, cfg, "aws dynamodb delete-table --table-name T")
	if r.Allowed || !strings.Contains(r.Blocked[0], "still being created") {
		t.Fatalf("got %+v", r)
	}
}

func TestTableDeleteEdgeCases(t *testing.T) {
	f, cf, cfg := setup(t)
	if r := protect(t, f, cf, cfg, "aws dynamodb delete-table --table-name Nope"); !r.Allowed || len(r.Undo) != 0 {
		t.Fatalf("missing table: %+v", r)
	}
	f.CreateTable("T", "id")
	f.Fail["create-backup"] = "An error occurred (LimitExceededException)"
	if r := protect(t, f, cf, cfg, "aws dynamodb delete-table --table-name T"); r.Allowed {
		t.Fatal("allowed without a backup")
	}
	// Settings that cannot be read do not stop the backup.
	delete(f.Fail, "create-backup")
	f.Fail["list-tags-of-resource"] = "An error occurred (AccessDeniedException)"
	r := protect(t, f, cf, cfg, "aws dynamodb delete-table --table-name T")
	if !r.Allowed || len(r.Undo[0].TableConfig.Unread) != 1 {
		t.Fatalf("got %+v", r)
	}
}

func TestItemsAreSavedBeforeChanges(t *testing.T) {
	cfg, run := ddbSetup(t)
	r := run(`aws dynamodb delete-item --table-name Users --key '{"pk":{"S":"u1"}}' && aws dynamodb put-item --table-name Users --item '{"pk":{"S":"u2"},"name":{"S":"New"}}'`)
	if !r.Allowed || len(r.Undo) != 2 {
		t.Fatalf("got %+v", r)
	}
	s := savedItems(t, r.Undo[0].ItemsFile)
	if len(s) != 1 || !strings.Contains(string(s[0].Item), "Ali") {
		t.Fatalf("saved = %s", s)
	}
	// u2 did not exist before: saved as null.
	s = savedItems(t, r.Undo[1].ItemsFile)
	if len(s) != 1 || string(s[0].Item) != "null" || compactJSON(s[0].Key) != `{"pk":{"S":"u2"}}` {
		t.Fatalf("saved = %+v", s)
	}
	if !strings.HasPrefix(r.Undo[0].ItemsFile, cfg.DataDir) {
		t.Fatalf("file outside the journal folder: %s", r.Undo[0].ItemsFile)
	}
	if fi, _ := os.Stat(r.Undo[0].ItemsFile); fi.Mode().Perm() != 0o600 {
		t.Fatalf("items file mode %v", fi.Mode().Perm())
	}
}

func TestItemLimitsAndErrors(t *testing.T) {
	cfg, run := ddbSetup(t)
	cfg.MaxItems = 1
	r := run(`aws dynamodb batch-write-item --request-items '{"Users":[{"DeleteRequest":{"Key":{"pk":{"S":"a"}}}},{"DeleteRequest":{"Key":{"pk":{"S":"b"}}}}]}'`)
	if r.Allowed {
		t.Fatal("allowed above the item limit")
	}

	_, run = ddbSetup(t)
	r = run(`aws dynamodb put-item --table-name Users --item '{"name":{"S":"no key"}}'`)
	if r.Allowed || !strings.Contains(r.Blocked[0], "key") {
		t.Fatalf("item without its key attribute: %+v", r)
	}

	f, cf, c := setup(t)
	c.DataDir = t.TempDir()
	f.CreateTable("T", "id")
	f.Fail["get-item"] = "An error occurred (AccessDeniedException)"
	if r := protect(t, f, cf, c, `aws dynamodb delete-item --table-name T --key '{"id":{"N":"1"}}'`); r.Allowed {
		t.Fatal("allowed although the old item could not be read")
	}
}

func TestDynamoBlocks(t *testing.T) {
	for _, line := range []string{
		`aws dynamodb execute-statement --statement "DELETE FROM Users"`,
		"aws dynamodb delete-item --table-name Users --key file://k.json",
		"aws dynamodb update-table --table-name Users --no-deletion-protection-enabled",
		"aws dynamodb update-time-to-live --table-name Users --time-to-live-specification Enabled=true,AttributeName=exp",
		"aws dynamodb delete-backup --backup-arn arn:x",
		"aws dynamodb delete-table --table-name $T",
	} {
		f, cf, cfg := setup(t)
		f.CreateTable("Users", "pk")
		r := protect(t, f, cf, cfg, line)
		if r.Allowed || len(f.Calls) != 0 {
			t.Errorf("%q: allowed=%v calls=%d", line, r.Allowed, len(f.Calls))
		}
	}
}

func TestBackupName(t *testing.T) {
	ts := time.Date(2026, 10, 18, 9, 0, 0, 0, time.UTC)
	if got := backupName("my.table_1-x", ts); got != "cloudundo-my.table_1-x-20261018T090000Z" {
		t.Fatal(got)
	}
	if got := backupName(strings.Repeat("a", 300), ts); len(got) != 255 {
		t.Fatalf("len %d", len(got))
	}
}

func compactJSON(b []byte) string {
	var out bytes.Buffer
	_ = json.Compact(&out, b)
	return out.String()
}
