package undo

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/cloudundo/cloudundo/internal/effects"
	"github.com/cloudundo/cloudundo/internal/journal"
	"github.com/cloudundo/cloudundo/internal/protect"
)

const (
	u1Key = `{"pk":{"S":"u1"}}`
	u1    = `{"pk":{"S":"u1"},"name":{"S":"Ali"}}`
	u2Key = `{"pk":{"S":"u2"}}`
	u2    = `{"pk":{"S":"u2"},"name":{"S":"Mona"}}`
)

// ddbWorld has a Users table with settings and two items.
func ddbWorld(t *testing.T) (*world, string) {
	w := newWorld(t)
	w.f.CreateTable("Users", "pk")
	w.f.SetTableSettings("Users", "expires", map[string]string{"team": "core"}, true, "NEW_AND_OLD_IMAGES")
	w.f.PutItem("Users", u1)
	w.f.PutItem("Users", u2)
	return w, t.TempDir()
}

func (w *world) protectDDB(dir, line string) journal.Entry {
	w.t.Helper()
	cfg := protect.Defaults()
	cfg.Now = w.f.Now
	cfg.DataDir = dir
	cfg.Sleep = func(time.Duration) {}
	r := protect.Run(context.Background(), effects.Analyze(line), w.cf, cfg)
	if !r.Allowed {
		w.t.Fatalf("%q blocked: %v", line, r.Blocked)
	}
	return journal.Entry{ID: 7, Command: line, Undo: r.Undo, Status: "protected"}
}

func (w *world) plan(e *journal.Entry, dir string) Plan {
	w.t.Helper()
	p, err := For(context.Background(), e, w.cf, dir)
	if err != nil {
		w.t.Fatalf("plan: %v", err)
	}
	return p
}

func (w *world) apply(p Plan) {
	w.t.Helper()
	if _, err := p.Apply(context.Background()); err != nil {
		w.t.Fatalf("apply: %v", err)
	}
}

func (w *world) wantItem(key, item string) {
	w.t.Helper()
	if got := w.f.Item("Users", key); !sameJSON(got, item) {
		w.t.Fatalf("item %s = %s, want %s", key, got, item)
	}
}

func TestUndoTableDelete(t *testing.T) {
	w, dir := ddbWorld(t)
	e := w.protectDDB(dir, "aws dynamodb delete-table --table-name Users")
	w.f.DropTable("Users") // the agent's command

	// Run 1: start the restore.
	p := w.plan(&e, dir)
	if !p.Pending || len(p.Actions) != 1 || !strings.Contains(p.Actions[0].Desc, "restore DynamoDB table Users") {
		t.Fatalf("run 1 plan = %+v", p)
	}
	w.apply(p)
	if !w.f.HasTable("Users") {
		t.Fatal("table not restored")
	}
	if e.Undo[0].RestoredID == "" || e.Undo[0].RestoredID != w.f.TableID("Users") {
		t.Fatalf("restored id not recorded: %q vs %q", e.Undo[0].RestoredID, w.f.TableID("Users"))
	}
	w.wantItem(u1Key, u1)
	w.wantItem(u2Key, u2)
	if ttl, tags, pitr, stream := w.f.TableSettings("Users"); ttl != "" || len(tags) != 0 || pitr || stream != "" {
		t.Fatal("fake restore should not bring settings back by itself")
	}

	// The entry goes through JSON in the journal between runs.
	b, _ := json.Marshal(e)
	var saved journal.Entry
	if err := json.Unmarshal(b, &saved); err != nil {
		t.Fatal(err)
	}

	// Run 2: put the settings back.
	p = w.plan(&saved, dir)
	if p.Pending {
		t.Fatalf("run 2 still pending: %+v", p)
	}
	w.apply(p)
	ttl, tags, pitr, stream := w.f.TableSettings("Users")
	if ttl != "expires" || tags["team"] != "core" || !pitr || stream != "NEW_AND_OLD_IMAGES" {
		t.Fatalf("settings = %q %v %v %q", ttl, tags, pitr, stream)
	}
	if !saved.Undo[0].SettingsApplied {
		t.Fatal("not marked as fully restored")
	}

	// Run 3: nothing left.
	if p := w.plan(&saved, dir); len(p.Actions) != 0 || p.Pending {
		t.Fatalf("run 3 plan = %+v", p)
	}
}

func TestUndoTableWaitsWhileRestoring(t *testing.T) {
	w, dir := ddbWorld(t)
	e := w.protectDDB(dir, "aws dynamodb delete-table --table-name Users")
	w.f.DropTable("Users")
	w.f.RestorePolls = 2

	w.apply(w.plan(&e, dir))
	for i := 0; i < 2; i++ {
		p := w.plan(&e, dir)
		if !p.Pending || len(p.Actions) != 0 || !strings.Contains(p.Summary(), "still being restored") {
			t.Fatalf("poll %d plan = %+v", i, p)
		}
	}
	p := w.plan(&e, dir)
	if p.Pending || len(p.Actions) == 0 {
		t.Fatalf("after restore plan = %+v", p)
	}
}

func TestUndoTableThatStillExists(t *testing.T) {
	w, dir := ddbWorld(t)
	e := w.protectDDB(dir, "aws dynamodb delete-table --table-name Users")
	// The delete never happened.
	p := w.plan(&e, dir)
	if len(p.Actions) != 0 || p.Pending || !strings.Contains(p.Summary(), "nothing to restore") {
		t.Fatalf("plan = %+v", p)
	}
}

func TestUndoTableLeavesANewTableAlone(t *testing.T) {
	w, dir := ddbWorld(t)
	e := w.protectDDB(dir, "aws dynamodb delete-table --table-name Users")
	w.f.DropTable("Users")
	w.apply(w.plan(&e, dir))

	// Someone deletes the restored table and makes a new one.
	w.f.DropTable("Users")
	w.f.CreateTable("Users", "pk")
	w.f.PutItem("Users", `{"pk":{"S":"new"}}`)
	p := w.plan(&e, dir)
	if len(p.Actions) != 0 || !strings.Contains(p.Summary(), "different table") {
		t.Fatalf("plan = %+v", p)
	}
}

func TestUndoTableBackupGone(t *testing.T) {
	w, dir := ddbWorld(t)
	e := w.protectDDB(dir, "aws dynamodb delete-table --table-name Users")
	w.f.DropTable("Users")
	e.Undo[0].BackupArn = "arn:aws:dynamodb:us-east-1:" + w.f.Account + ":table/Users/backup/missing"
	p := w.plan(&e, dir)
	if _, err := p.Apply(context.Background()); err == nil {
		t.Fatal("restore from a missing backup should fail")
	}
	if e.Undo[0].RestoredID != "" {
		t.Fatal("failed restore must not be recorded")
	}
}

func TestUndoDeletedItem(t *testing.T) {
	w, dir := ddbWorld(t)
	e := w.protectDDB(dir, `aws dynamodb delete-item --table-name Users --key '`+u1Key+`'`)
	w.f.DeleteItem("Users", u1Key)

	p := w.plan(&e, dir)
	if len(p.Actions) != 1 || !strings.Contains(p.Actions[0].Desc, "put back") {
		t.Fatalf("plan = %+v", p)
	}
	w.apply(p)
	w.wantItem(u1Key, u1)
	w.wantItem(u2Key, u2)

	if p := w.plan(&e, dir); len(p.Actions) != 0 {
		t.Fatalf("second undo should do nothing: %+v", p)
	}
}

func TestUndoOverwrittenItemKeepsTheNewValue(t *testing.T) {
	w, dir := ddbWorld(t)
	changed := `{"pk":{"S":"u1"},"name":{"S":"HACKED"}}`
	e := w.protectDDB(dir, `aws dynamodb put-item --table-name Users --item '`+changed+`'`)
	w.f.PutItem("Users", changed)

	p := w.plan(&e, dir)
	if len(p.Actions) != 2 {
		t.Fatalf("plan = %+v", p)
	}
	w.apply(p)
	w.wantItem(u1Key, u1)

	// The value undo replaced was saved first.
	files, _ := filepath.Glob(filepath.Join(dir, "items", "undo-7-Users-*.json"))
	if len(files) != 1 {
		t.Fatalf("undo save files = %v", files)
	}
	body, _ := os.ReadFile(files[0])
	var cur []journal.SavedItem
	if err := json.Unmarshal(body, &cur); err != nil || len(cur) != 1 || !sameJSON(string(cur[0].Item), changed) {
		t.Fatalf("saved current = %s (%v)", body, err)
	}
	if st, _ := os.Stat(files[0]); st.Mode().Perm() != 0o600 {
		t.Fatalf("mode = %v", st.Mode())
	}
}

func TestUndoLeavesNewItems(t *testing.T) {
	w, dir := ddbWorld(t)
	fresh := `{"pk":{"S":"u9"},"name":{"S":"New"}}`
	e := w.protectDDB(dir, `aws dynamodb put-item --table-name Users --item '`+fresh+`'`)
	w.f.PutItem("Users", fresh)

	p := w.plan(&e, dir)
	if len(p.Actions) != 0 || !strings.Contains(p.Summary(), "did not exist before") {
		t.Fatalf("plan = %+v", p)
	}
	if w.f.Item("Users", `{"pk":{"S":"u9"}}`) == "" {
		t.Fatal("undo must never delete items")
	}
}

func TestUndoBatchDelete(t *testing.T) {
	w, dir := ddbWorld(t)
	req := `{"Users":[{"DeleteRequest":{"Key":` + u1Key + `}},{"DeleteRequest":{"Key":` + u2Key + `}}]}`
	e := w.protectDDB(dir, `aws dynamodb batch-write-item --request-items '`+req+`'`)
	w.f.DeleteItem("Users", u1Key)
	w.f.DeleteItem("Users", u2Key)

	w.apply(w.plan(&e, dir))
	w.wantItem(u1Key, u1)
	w.wantItem(u2Key, u2)
}

func TestUndoItemsWhenTableIsGone(t *testing.T) {
	w, dir := ddbWorld(t)
	e := w.protectDDB(dir, `aws dynamodb delete-item --table-name Users --key '`+u1Key+`'`)
	w.f.DropTable("Users")
	if _, err := For(context.Background(), &e, w.cf, dir); err == nil || !strings.Contains(err.Error(), "does not exist") {
		t.Fatalf("err = %v", err)
	}
}

func TestUndoItemsFileMissing(t *testing.T) {
	w, dir := ddbWorld(t)
	e := w.protectDDB(dir, `aws dynamodb delete-item --table-name Users --key '`+u1Key+`'`)
	os.Remove(e.Undo[0].ItemsFile)
	if _, err := For(context.Background(), &e, w.cf, dir); err == nil || !strings.Contains(err.Error(), "missing") {
		t.Fatalf("err = %v", err)
	}
}

func TestRestoreName(t *testing.T) {
	for in, want := range map[string]string{
		"Users": "Users",
		"arn:aws:dynamodb:us-east-1:123456789012:table/Users": "Users",
	} {
		if got := restoreName(in); got != want {
			t.Errorf("restoreName(%q) = %q", in, got)
		}
	}
}
