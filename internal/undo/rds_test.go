package undo

import (
	"context"
	"encoding/json"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/cloudundo-dev/cloudundo/internal/awscli"
	"github.com/cloudundo-dev/cloudundo/internal/effects"
	"github.com/cloudundo-dev/cloudundo/internal/journal"
	"github.com/cloudundo-dev/cloudundo/internal/protect"
)

var prodDB = awscli.DBInfo{
	ID: "prod-db", Engine: "postgres", EngineVersion: "16.4", Class: "db.t4g.micro",
	SubnetGroup: "app-subnets", SecurityGroups: []string{"sg-1", "sg-2"},
	ParameterGroup: "pg16-custom", OptionGroup: "default:postgres-16", Port: 5433,
	MultiAZ: true, StorageType: "gp3", BackupRetention: 7,
	Tags: []awscli.Tag{{Key: "team", Value: "core"}},
}

func (w *world) protectRDS(line string) (*journal.Entry, protect.Result) {
	w.t.Helper()
	cfg := protect.Defaults()
	cfg.Now = w.f.Now
	cfg.Sleep = func(time.Duration) {}
	r := protect.Run(context.Background(), effects.Analyze(line), w.cf, cfg)
	if !r.Allowed {
		w.t.Fatalf("%q blocked: %v", line, r.Blocked)
	}
	return &journal.Entry{ID: 9, Command: line, Undo: r.Undo, Status: "protected"}, r
}

// roundTrip saves and loads the entry, as the journal does between runs.
func roundTrip(t *testing.T, e *journal.Entry) *journal.Entry {
	b, _ := json.Marshal(e)
	var out journal.Entry
	if err := json.Unmarshal(b, &out); err != nil {
		t.Fatal(err)
	}
	return &out
}

func TestUndoDBDeleteWithoutFinalSnapshot(t *testing.T) {
	w := newWorld(t)
	w.f.CreateDB(prodDB, "orders: 1200 rows")
	e, r := w.protectRDS("aws rds delete-db-instance --db-instance-identifier prod-db --skip-final-snapshot")
	if len(r.Undo) != 1 || !strings.Contains(strings.Join(r.Protection, " "), "made snapshot cloudundo-prod-db-") {
		t.Fatalf("protect = %+v", r)
	}
	w.f.DropDB("prod-db", false, "") // the agent's command

	// Run 1: start the restore.
	p := w.plan(e, "")
	if !p.Pending || len(p.Actions) != 1 || !strings.Contains(p.Actions[0].Desc, "restore database prod-db") {
		t.Fatalf("run 1 = %+v", p)
	}
	w.apply(p)
	got, data, ok := w.f.DB("prod-db", false)
	if !ok || data != "orders: 1200 rows" {
		t.Fatalf("restored = %v %q", ok, data)
	}
	want := prodDB
	for _, f := range []struct {
		name      string
		got, want any
	}{
		{"class", got.Class, want.Class}, {"subnets", got.SubnetGroup, want.SubnetGroup},
		{"security groups", got.SecurityGroups, want.SecurityGroups}, {"parameters", got.ParameterGroup, want.ParameterGroup},
		{"options", got.OptionGroup, want.OptionGroup}, {"port", got.Port, want.Port},
		{"multi-az", got.MultiAZ, want.MultiAZ}, {"storage", got.StorageType, want.StorageType},
		{"tags", got.Tags, want.Tags},
	} {
		if !reflect.DeepEqual(f.got, f.want) {
			t.Errorf("%s = %v, want %v", f.name, f.got, f.want)
		}
	}

	// Run 2 (after a save and load): backup retention back to 7 days.
	e = roundTrip(t, e)
	p = w.plan(e, "")
	if p.Pending {
		t.Fatalf("run 2 still pending: %+v", p)
	}
	w.apply(p)
	if got, _, _ := w.f.DB("prod-db", false); got.BackupRetention != 7 {
		t.Fatalf("retention = %d", got.BackupRetention)
	}
	if p := w.plan(e, ""); len(p.Actions) != 0 || p.Pending {
		t.Fatalf("run 3 = %+v", p)
	}
}

func TestUndoDBDeleteWithFinalSnapshot(t *testing.T) {
	w := newWorld(t)
	w.f.CreateDB(prodDB, "v1")
	e, r := w.protectRDS("aws rds delete-db-instance --db-instance-identifier prod-db --final-db-snapshot-identifier last-one")
	if calls := strings.Join(callNames(w), " "); strings.Contains(calls, "create-db-snapshot") {
		t.Fatalf("took its own snapshot although AWS takes one: %s", calls)
	}
	if !strings.Contains(strings.Join(r.Protection, " "), "final snapshot last-one") {
		t.Fatalf("protect = %+v", r)
	}
	w.f.DropDB("prod-db", false, "last-one")
	w.apply(w.plan(e, ""))
	if _, data, ok := w.f.DB("prod-db", false); !ok || data != "v1" {
		t.Fatalf("restored = %v %q", ok, data)
	}
}

func callNames(w *world) []string {
	var out []string
	for _, c := range w.f.Calls {
		if len(c) > 1 {
			out = append(out, c[1])
		}
	}
	return out
}

func TestUndoDBWaitsForRestore(t *testing.T) {
	w := newWorld(t)
	w.f.CreateDB(prodDB, "v1")
	e, _ := w.protectRDS("aws rds delete-db-instance --db-instance-identifier prod-db --skip-final-snapshot")
	w.f.DropDB("prod-db", false, "")
	w.f.RestorePolls = 2
	w.apply(w.plan(e, ""))
	for i := 0; i < 2; i++ {
		if p := w.plan(e, ""); !p.Pending || len(p.Actions) != 0 || !strings.Contains(p.Summary(), "still being restored") {
			t.Fatalf("poll %d = %+v", i, p)
		}
	}
	if p := w.plan(e, ""); p.Pending || len(p.Actions) == 0 {
		t.Fatalf("after = %+v", p)
	}
}

func TestUndoDBLeavesExistingDatabasesAlone(t *testing.T) {
	w := newWorld(t)
	w.f.CreateDB(prodDB, "v1")
	e, _ := w.protectRDS("aws rds delete-db-instance --db-instance-identifier prod-db --skip-final-snapshot")
	// The delete never happened.
	if p := w.plan(e, ""); len(p.Actions) != 0 || !strings.Contains(p.Summary(), "nothing to restore") {
		t.Fatalf("plan = %+v", p)
	}
	// Restored, then someone replaced it with a new database.
	w.f.DropDB("prod-db", false, "")
	w.apply(w.plan(e, ""))
	w.f.DropDB("prod-db", false, "")
	w.f.CreateDB(prodDB, "someone else's")
	if p := w.plan(e, ""); len(p.Actions) != 0 || !strings.Contains(p.Summary(), "different database") {
		t.Fatalf("plan = %+v", p)
	}
}

func TestUndoDBSnapshotGone(t *testing.T) {
	w := newWorld(t)
	w.f.CreateDB(prodDB, "v1")
	e, _ := w.protectRDS("aws rds delete-db-instance --db-instance-identifier prod-db --skip-final-snapshot")
	w.f.DropDB("prod-db", false, "")
	e.Undo[0].Snapshot = "missing"
	if _, err := For(context.Background(), e, w.cf, ""); err == nil || !strings.Contains(err.Error(), "is gone") {
		t.Fatalf("err = %v", err)
	}
}

func TestUndoAuroraCluster(t *testing.T) {
	w := newWorld(t)
	w.f.CreateDB(awscli.DBInfo{ID: "app", Cluster: true, Engine: "aurora-postgresql", EngineVersion: "16.4",
		SubnetGroup: "app-subnets", SecurityGroups: []string{"sg-1"}, ParameterGroup: "aurora-pg16", Port: 5432, BackupRetention: 5}, "users")
	w.f.CreateDB(awscli.DBInfo{ID: "app-1", ClusterID: "app", Engine: "aurora-postgresql", Class: "db.r6g.large"}, "")
	w.f.CreateDB(awscli.DBInfo{ID: "app-2", ClusterID: "app", Engine: "aurora-postgresql", Class: "db.r6g.xlarge"}, "")

	line := "aws rds delete-db-instance --db-instance-identifier app-1 --skip-final-snapshot && " +
		"aws rds delete-db-instance --db-instance-identifier app-2 --skip-final-snapshot && " +
		"aws rds delete-db-cluster --db-cluster-identifier app --skip-final-snapshot"
	e, r := w.protectRDS(line)
	if len(r.Undo) != 1 || !strings.Contains(strings.Join(r.Notes, " "), "data stays in the cluster") {
		t.Fatalf("protect = %+v", r)
	}
	w.f.DropDB("app-1", false, "")
	w.f.DropDB("app-2", false, "")
	w.f.DropDB("app", true, "")

	w.apply(w.plan(e, ""))
	if _, data, ok := w.f.DB("app", true); !ok || data != "users" {
		t.Fatalf("cluster = %v %q", ok, data)
	}
	p := w.plan(e, "")
	w.apply(p)
	for id, class := range map[string]string{"app-1": "db.r6g.large", "app-2": "db.r6g.xlarge"} {
		got, _, ok := w.f.DB(id, false)
		if !ok || got.Class != class || got.ClusterID != "app" {
			t.Fatalf("%s = %+v %v", id, got, ok)
		}
	}
	if got, _, _ := w.f.DB("app", true); got.BackupRetention != 5 {
		t.Fatalf("cluster retention = %d", got.BackupRetention)
	}
}

func TestUndoDBWaitsForTheFinalSnapshot(t *testing.T) {
	w := newWorld(t)
	w.f.CreateDB(prodDB, "v1")
	e, _ := w.protectRDS("aws rds delete-db-instance --db-instance-identifier prod-db --final-db-snapshot-identifier last-one")
	w.f.SnapshotPolls = 1 // AWS is still writing the final snapshot
	w.f.DropDB("prod-db", false, "last-one")
	if p := w.plan(e, ""); !p.Pending || len(p.Actions) != 0 || !strings.Contains(p.Summary(), "still being made") {
		t.Fatalf("plan = %+v", p)
	}
	w.apply(w.plan(e, ""))
	if _, data, ok := w.f.DB("prod-db", false); !ok || data != "v1" {
		t.Fatalf("restored = %v %q", ok, data)
	}
}
