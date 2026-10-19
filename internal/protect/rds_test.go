package protect

import (
	"strings"
	"testing"
	"time"

	"github.com/cloudundo/cloudundo/internal/awscli"
	"github.com/cloudundo/cloudundo/internal/effects"
)

func rdsSetup(t *testing.T) (*Config, func(string) Result, func() []string) {
	t.Helper()
	f, cf, cfg := setup(t)
	cfg.Sleep = func(time.Duration) {}
	f.CreateDB(awscli.DBInfo{ID: "db1", Engine: "postgres", Class: "db.t4g.micro", BackupRetention: 7}, "rows")
	rdsFake = f
	run := func(line string) Result { return protect(t, f, cf, cfg, line) }
	names := func() []string {
		var out []string
		for _, c := range f.Calls {
			out = append(out, c[1])
		}
		return out
	}
	return &cfg, run, names
}

var rdsFake interface {
	HasDBSnapshot(string) bool
}

func TestDBDeleteTakesASnapshotFirst(t *testing.T) {
	_, run, _ := rdsSetup(t)
	r := run("aws rds delete-db-instance --db-instance-identifier db1 --skip-final-snapshot")
	if !r.Allowed || len(r.Undo) != 1 || r.Undo[0].Kind != "rds-snapshot" || r.Undo[0].DB.Class != "db.t4g.micro" {
		t.Fatalf("r = %+v", r)
	}
	if !rdsFake.HasDBSnapshot(r.Undo[0].Snapshot) || !strings.HasPrefix(r.Undo[0].Snapshot, "cloudundo-db1-") {
		t.Fatalf("snapshot %q missing", r.Undo[0].Snapshot)
	}
}

func TestDBSnapshotTooSlowBlocksThenIsReused(t *testing.T) {
	f, cf, cfg := setup(t)
	cfg.Sleep = func(time.Duration) {}
	cfg.SnapshotWait, cfg.SnapshotPoll = 30*time.Second, 10*time.Second
	f.CreateDB(awscli.DBInfo{ID: "db1", Engine: "postgres", Class: "db.t4g.micro"}, "rows")
	f.SnapshotPolls = 5 // longer than the 30 s wait, ready soon after
	line := "aws rds delete-db-instance --db-instance-identifier db1 --skip-final-snapshot"

	r := protect(t, f, cf, cfg, line)
	if r.Allowed || !strings.Contains(strings.Join(r.Blocked, " "), "still being made") {
		t.Fatalf("first try = %+v", r)
	}
	before := calls(f, "create-db-snapshot")
	r = protect(t, f, cf, cfg, line)
	if !r.Allowed || calls(f, "create-db-snapshot") != before {
		t.Fatalf("retry = %+v (created another snapshot: %v)", r, calls(f, "create-db-snapshot") != before)
	}
	if !strings.Contains(strings.Join(r.Notes, " "), "Using snapshot cloudundo-db1-") {
		t.Fatalf("notes = %v", r.Notes)
	}
}

func TestDBDeleteSpecialCases(t *testing.T) {
	f, cf, cfg := setup(t)
	cfg.Sleep = func(time.Duration) {}
	f.CreateDB(awscli.DBInfo{ID: "locked", Engine: "mysql", DeletionProtection: true}, "")
	f.CreateDB(awscli.DBInfo{ID: "c", Cluster: true, Engine: "aurora-mysql"}, "")
	f.CreateDB(awscli.DBInfo{ID: "c-1", ClusterID: "c", Engine: "aurora-mysql"}, "")
	for line, note := range map[string]string{
		"aws rds delete-db-instance --db-instance-identifier nope --skip-final-snapshot":   "does not exist",
		"aws rds delete-db-instance --db-instance-identifier locked --skip-final-snapshot": "deletion protection on",
		"aws rds delete-db-instance --db-instance-identifier c-1 --skip-final-snapshot":    "data stays in the cluster",
		"aws rds delete-db-instance --db-instance-identifier c-1":                          "data stays in the cluster",
	} {
		r := protect(t, f, cf, cfg, line)
		if !r.Allowed || len(r.Undo) != 0 || !strings.Contains(strings.Join(r.Notes, " "), note) {
			t.Errorf("%s: %+v", line, r)
		}
	}
	if calls(f, "create-db-snapshot") != 0 {
		t.Fatal("snapshot taken where none was needed")
	}
}

func TestDBBlocks(t *testing.T) {
	f, cf, cfg := setup(t)
	for _, line := range []string{
		"aws rds delete-db-instance --db-instance-identifier $DB --skip-final-snapshot",
		"aws rds delete-db-snapshot --db-snapshot-identifier nightly",
		"aws rds modify-db-instance --db-instance-identifier db1 --no-deletion-protection",
		"aws rds modify-db-instance --db-instance-identifier db1 --backup-retention-period 0",
		"aws rds delete-db-cluster-automated-backup --db-cluster-resource-id x",
	} {
		if r := protect(t, f, cf, cfg, line); r.Allowed {
			t.Errorf("%s allowed", line)
		}
	}
	if len(f.Calls) != 0 {
		t.Fatalf("AWS called: %v", f.Calls)
	}
}

func TestDBSnapshotFailureBlocks(t *testing.T) {
	f, cf, cfg := setup(t)
	cfg.Sleep = func(time.Duration) {}
	f.CreateDB(awscli.DBInfo{ID: "db1", Engine: "postgres"}, "rows")
	f.Fail["create-db-snapshot"] = "An error occurred (AccessDenied)"
	if r := protect(t, f, cf, cfg, "aws rds delete-db-instance --db-instance-identifier db1 --skip-final-snapshot"); r.Allowed {
		t.Fatalf("allowed: %+v", r)
	}
}

func TestCacheRules(t *testing.T) {
	f, cf, cfg := setup(t)
	f.CreateCache("sessions", "redis", "", false)
	f.CreateCache("node-2", "redis", "grp", false)
	f.CreateCache("mc", "memcached", "", false)
	f.CreateCache("sless", "valkey", "", true)
	allowed := map[string]string{
		"aws elasticache delete-cache-cluster --cache-cluster-id mc":                                           "Memcached",
		"aws elasticache delete-cache-cluster --cache-cluster-id node-2":                                       "stays in the group",
		"aws elasticache delete-cache-cluster --cache-cluster-id sessions --final-snapshot-identifier s1":      "final snapshot s1",
		"aws elasticache delete-replication-group --replication-group-id grp --final-snapshot-identifier last": "final snapshot last",
		"aws elasticache delete-cache-cluster --cache-cluster-id gone":                                         "does not exist",
	}
	for line, note := range allowed {
		r := protect(t, f, cf, cfg, line)
		if !r.Allowed || !strings.Contains(strings.Join(r.Notes, " "), note) {
			t.Errorf("%s: %+v", line, r)
		}
	}
	blocked := map[string]string{
		"aws elasticache delete-cache-cluster --cache-cluster-id sessions":                                 "--final-snapshot-identifier",
		"aws elasticache delete-replication-group --replication-group-id grp":                              "--final-snapshot-identifier",
		"aws elasticache delete-serverless-cache --serverless-cache-name sless":                            "--final-snapshot-name",
		"aws elasticache delete-snapshot --snapshot-name nightly":                                          "deletes a cache snapshot",
		"redis-cli -h sessions.cache.amazonaws.com FLUSHALL":                                               "FLUSHALL",
		"aws elasticache modify-replication-group --replication-group-id grp --snapshot-retention-limit 0": "automatic backups",
	}
	for line, why := range blocked {
		r := protect(t, f, cf, cfg, line)
		if r.Allowed || !strings.Contains(strings.Join(r.Blocked, " "), why) {
			t.Errorf("%s: %+v", line, r)
		}
	}
}

func TestUnknownKindIsBlocked(t *testing.T) {
	_, cf, cfg := setup(t)
	r := Run(t.Context(), []effects.Effect{{Kind: "delete-something-new", Service: "s3", Bucket: "b", Command: "x"}}, cf, cfg)
	if r.Allowed {
		t.Fatal("an effect cloudundo cannot protect was allowed")
	}
}
