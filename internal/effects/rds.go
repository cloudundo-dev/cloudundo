package effects

import (
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
)

// RDS and ElastiCache kinds.
const (
	// DeleteDatabase removes an RDS instance, or an Aurora cluster when
	// Cluster is set, and its data.
	DeleteDatabase Kind = "delete-database"
	// DeleteCache removes an ElastiCache cluster, replication group or
	// serverless cache (CacheKind) and its data.
	DeleteCache Kind = "delete-cache"
	// EraseCacheData empties a Redis or Valkey server from inside
	// (FLUSHALL, FLUSHDB). Nothing outside the server can bring it back.
	EraseCacheData Kind = "erase-cache-data"
	// UnprotectedData deletes stored data cloudundo cannot back up yet
	// (an EBS volume or EFS file system in a terraform plan, say).
	UnprotectedData Kind = "unprotected-data"
)

// Cache kinds.
const (
	CacheCluster          = "cache-cluster"
	CacheReplicationGroup = "replication-group"
	CacheServerless       = "serverless"
)

// rds handles "aws rds <operation>".
func rds(p parsed) []Effect {
	op := p.pos[1]
	weaken := func(id, detail string) []Effect {
		return []Effect{{Kind: WeakenProtection, Database: id, Detail: detail}}
	}
	switch op {
	case "delete-db-instance":
		return []Effect{dbDelete(p, p.value("db-instance-identifier"), false)}
	case "delete-db-cluster":
		return []Effect{dbDelete(p, p.value("db-cluster-identifier"), true)}

	case "delete-db-snapshot":
		return weaken(p.value("db-snapshot-identifier"), "deletes a database snapshot")
	case "delete-db-cluster-snapshot":
		return weaken(p.value("db-cluster-snapshot-identifier"), "deletes a database snapshot")
	case "delete-db-instance-automated-backup", "delete-db-cluster-automated-backup":
		return weaken("?", "deletes a database's automated backups")

	case "modify-db-instance", "modify-db-cluster":
		id := p.value("db-instance-identifier")
		if op == "modify-db-cluster" {
			id = p.value("db-cluster-identifier")
		}
		if p.has("no-deletion-protection") {
			return weaken(id, "turns off deletion protection")
		}
		if p.has("backup-retention-period") {
			if n, err := strconv.Atoi(p.value("backup-retention-period")); err != nil || n == 0 {
				return weaken(id, "turns off automated backups, which deletes the existing ones")
			}
		}
	}
	return nil
}

func dbDelete(p parsed, id string, cluster bool) Effect {
	e := Effect{Kind: DeleteDatabase, Database: id, Cluster: cluster,
		FinalSnapshot: p.value("final-db-snapshot-identifier")}
	e.SkipFinalSnapshot = p.has("skip-final-snapshot") && !p.has("no-skip-final-snapshot")
	if e.SkipFinalSnapshot {
		e.FinalSnapshot = ""
	}
	if !e.SkipFinalSnapshot && e.FinalSnapshot != "" && !validName(e.FinalSnapshot) {
		e.Uncertain = true
		e.Detail = "the final snapshot name is not written in the command"
	}
	return e
}

// elasticache handles "aws elasticache <operation>".
func elasticache(p parsed) []Effect {
	op := p.pos[1]
	weaken := func(id, detail string) []Effect {
		return []Effect{{Kind: WeakenProtection, Cache: id, Detail: detail}}
	}
	switch op {
	case "delete-cache-cluster":
		return []Effect{{Kind: DeleteCache, CacheKind: CacheCluster, Cache: p.value("cache-cluster-id"),
			FinalSnapshot: p.value("final-snapshot-identifier")}}
	case "delete-replication-group":
		e := Effect{Kind: DeleteCache, CacheKind: CacheReplicationGroup, Cache: p.value("replication-group-id"),
			FinalSnapshot: p.value("final-snapshot-identifier")}
		if p.has("retain-primary-cluster") && !p.has("no-retain-primary-cluster") {
			// The data stays in the primary cluster.
			return nil
		}
		return []Effect{e}
	case "delete-serverless-cache":
		return []Effect{{Kind: DeleteCache, CacheKind: CacheServerless, Cache: p.value("serverless-cache-name"),
			FinalSnapshot: p.value("final-snapshot-name")}}

	case "delete-snapshot":
		return weaken(p.value("snapshot-name"), "deletes a cache snapshot")
	case "delete-serverless-cache-snapshot":
		return weaken(p.value("serverless-cache-snapshot-name"), "deletes a cache snapshot")

	case "modify-replication-group", "modify-cache-cluster", "modify-serverless-cache":
		if p.has("snapshot-retention-limit") {
			if n, err := strconv.Atoi(p.value("snapshot-retention-limit")); err != nil || n == 0 {
				id := p.value("replication-group-id") + p.value("cache-cluster-id") + p.value("serverless-cache-name")
				return weaken(id, "turns off automatic backups")
			}
		}
	}
	return nil
}

// redisClients run commands against a Redis-compatible server directly.
var redisClients = map[string]bool{"redis-cli": true, "valkey-cli": true, "keydb-cli": true}

// redisCLI finds FLUSHALL and FLUSHDB in a redis-cli invocation.
func redisCLI(args []string) []Effect {
	if len(args) == 0 || !redisClients[filepath.Base(args[0])] {
		return nil
	}
	for _, a := range args[1:] {
		switch strings.ToLower(a) {
		case "flushall", "flushdb":
			return []Effect{{Kind: EraseCacheData, Service: "redis", Command: strings.Join(args, " "),
				Detail: strings.ToUpper(a) + " empties the server from inside; no backup tool can bring that back"}}
		}
	}
	return nil
}

// validName is an RDS or ElastiCache identifier written out in full.
var nameRe = regexp.MustCompile(`^[A-Za-z][A-Za-z0-9-]{0,254}$`)

func validName(s string) bool { return nameRe.MatchString(s) }
