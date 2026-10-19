package effects

import (
	"reflect"
	"testing"
)

func TestAnalyzeRDSAndCaches(t *testing.T) {
	tests := []struct {
		name string
		line string
		want []Effect
	}{
		{
			name: "delete instance without a final snapshot",
			line: "aws rds delete-db-instance --db-instance-identifier prod-db --skip-final-snapshot",
			want: []Effect{{Kind: DeleteDatabase, Database: "prod-db", SkipFinalSnapshot: true}},
		},
		{
			name: "delete instance with a final snapshot",
			line: "aws rds delete-db-instance --db-instance-identifier prod-db --final-db-snapshot-identifier prod-db-final --delete-automated-backups",
			want: []Effect{{Kind: DeleteDatabase, Database: "prod-db", FinalSnapshot: "prod-db-final"}},
		},
		{
			name: "a skip flag before the id does not eat it",
			line: "aws rds delete-db-instance --skip-final-snapshot --db-instance-identifier prod-db",
			want: []Effect{{Kind: DeleteDatabase, Database: "prod-db", SkipFinalSnapshot: true}},
		},
		{
			name: "delete aurora cluster",
			line: "aws rds delete-db-cluster --db-cluster-identifier app --skip-final-snapshot",
			want: []Effect{{Kind: DeleteDatabase, Database: "app", Cluster: true, SkipFinalSnapshot: true}},
		},
		{
			name: "database name in a variable",
			line: "aws rds delete-db-instance --db-instance-identifier $DB --skip-final-snapshot",
			want: []Effect{{Kind: DeleteDatabase, Database: "$DB", SkipFinalSnapshot: true, Uncertain: true}},
		},
		{
			name: "final snapshot name in a variable",
			line: "aws rds delete-db-instance --db-instance-identifier db1 --final-db-snapshot-identifier $SNAP",
			want: []Effect{{Kind: DeleteDatabase, Database: "db1", FinalSnapshot: "$SNAP", Uncertain: true,
				Detail: "the final snapshot name is not written in the command"}},
		},
		{
			name: "deleting snapshots and backups weakens protection",
			line: "aws rds delete-db-snapshot --db-snapshot-identifier s1; aws rds delete-db-instance-automated-backup --dbi-resource-id x",
			want: []Effect{
				{Kind: WeakenProtection, Database: "s1", Detail: "deletes a database snapshot"},
				{Kind: WeakenProtection, Database: "?", Uncertain: true, Detail: "deletes a database's automated backups"},
			},
		},
		{
			name: "turning off deletion protection and backups",
			line: "aws rds modify-db-instance --db-instance-identifier db1 --no-deletion-protection --apply-immediately && aws rds modify-db-instance --db-instance-identifier db1 --backup-retention-period 0",
			want: []Effect{
				{Kind: WeakenProtection, Database: "db1", Detail: "turns off deletion protection"},
				{Kind: WeakenProtection, Database: "db1", Detail: "turns off automated backups, which deletes the existing ones"},
			},
		},
		{
			name: "ordinary rds changes are not data loss",
			line: "aws rds modify-db-instance --db-instance-identifier db1 --backup-retention-period 7 --apply-immediately && aws rds describe-db-instances && aws rds stop-db-instance --db-instance-identifier db1",
			want: nil,
		},
		{
			name: "elasticache deletes",
			line: "aws elasticache delete-replication-group --replication-group-id sessions && aws elasticache delete-cache-cluster --cache-cluster-id c1 --final-snapshot-identifier c1-final && aws elasticache delete-serverless-cache --serverless-cache-name s1",
			want: []Effect{
				{Kind: DeleteCache, CacheKind: CacheReplicationGroup, Cache: "sessions"},
				{Kind: DeleteCache, CacheKind: CacheCluster, Cache: "c1", FinalSnapshot: "c1-final"},
				{Kind: DeleteCache, CacheKind: CacheServerless, Cache: "s1"},
			},
		},
		{
			name: "keeping the primary keeps the data",
			line: "aws elasticache delete-replication-group --replication-group-id sessions --retain-primary-cluster",
			want: nil,
		},
		{
			name: "cache snapshots and retention",
			line: "aws elasticache delete-snapshot --snapshot-name nightly && aws elasticache modify-replication-group --replication-group-id g --snapshot-retention-limit 0",
			want: []Effect{
				{Kind: WeakenProtection, Cache: "nightly", Detail: "deletes a cache snapshot"},
				{Kind: WeakenProtection, Cache: "g", Detail: "turns off automatic backups"},
			},
		},
		{
			name: "redis-cli flushall",
			line: "redis-cli -h cache.local -p 6379 FlushAll",
			want: []Effect{{Kind: EraseCacheData, Detail: "FLUSHALL empties the server from inside; no backup tool can bring that back"}},
		},
		{
			name: "redis-cli reads are fine",
			line: "redis-cli -h cache.local get user:1 && valkey-cli ping",
			want: nil,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := strip(Analyze(tt.line))
			if !reflect.DeepEqual(got, tt.want) {
				t.Fatalf("Analyze(%q)\n got  %+v\n want %+v", tt.line, got, tt.want)
			}
		})
	}
}

func TestRDSServiceAndConn(t *testing.T) {
	e := Analyze("AWS_PROFILE=prod aws rds delete-db-instance --db-instance-identifier d --skip-final-snapshot --region eu-west-1")
	if len(e) != 1 || e[0].Service != "rds" || e[0].Conn.Region != "eu-west-1" {
		t.Fatalf("%+v", e)
	}
	if e := Analyze("redis-cli flushdb"); len(e) != 1 || e[0].Service != "redis" {
		t.Fatalf("%+v", e)
	}
}
