package protect

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/cloudundo-dev/cloudundo/internal/awscli"
	"github.com/cloudundo-dev/cloudundo/internal/effects"
	"github.com/cloudundo-dev/cloudundo/internal/journal"
)

// reuseWindow is how old a cloudundo snapshot may be and still protect a
// retried delete (the first try ran out of time waiting for it).
const reuseWindow = 30 * time.Minute

// dbSnapshot makes sure a deleted database can be restored: it uses the
// final snapshot the command asks AWS for, or takes one itself and waits
// until it is usable.
func (r *Result) dbSnapshot(ctx context.Context, c awscli.Client, e effects.Effect, since time.Time, cfg Config) bool {
	block := func(format string, a ...any) bool {
		r.Blocked = append(r.Blocked, fmt.Sprintf(format, a...))
		return false
	}
	what := "database " + e.Database
	if e.Cluster {
		what = "Aurora cluster " + e.Database
	}
	info, ok, err := c.DescribeDB(ctx, e.Database, e.Cluster)
	if err != nil {
		return block("could not check %s: %v", what, err)
	}
	if !ok {
		r.Notes = append(r.Notes, fmt.Sprintf("%s does not exist, so the command will fail on its own.", what))
		return true
	}
	if info.ClusterID != "" {
		r.Notes = append(r.Notes, fmt.Sprintf("%s is an instance of Aurora cluster %s; its data stays in the cluster.", e.Database, info.ClusterID))
		return true
	}
	if info.DeletionProtection {
		r.Notes = append(r.Notes, fmt.Sprintf("%s has deletion protection on, so AWS will refuse to delete it.", what))
		return true
	}
	if !e.SkipFinalSnapshot && e.FinalSnapshot == "" {
		r.Notes = append(r.Notes, "The command names no final snapshot and does not skip one, so AWS will refuse it.")
		return true
	}
	if e.Cluster {
		// Record each instance's size so undo can bring the cluster back
		// with the same instances.
		for i, m := range info.Members {
			if mi, ok, err := c.DescribeDB(ctx, m.ID, false); err == nil && ok {
				info.Members[i].Class = mi.Class
			}
		}
	}

	step := journal.UndoStep{Kind: "rds-snapshot", Since: since, Conn: e.Conn, DB: &info}
	if e.FinalSnapshot != "" {
		// AWS takes this snapshot as part of the delete; undo waits for it.
		step.Snapshot = e.FinalSnapshot
		r.Protection = append(r.Protection, fmt.Sprintf("AWS will keep final snapshot %s of %s", e.FinalSnapshot, what))
		r.Undo = append(r.Undo, step)
		return true
	}

	snap, err := r.recentSnapshot(ctx, c, e, since)
	if err != nil {
		return block("could not list the snapshots of %s: %v", what, err)
	}
	status := ""
	if snap.ID != "" {
		status = snap.Status
		r.Notes = append(r.Notes, fmt.Sprintf("Using snapshot %s, taken %s ago for this delete.", snap.ID, since.Sub(snap.Created).Round(time.Second)))
	} else {
		snap.ID = dbSnapshotName(e.Database, since)
		if status, err = c.CreateDBSnapshot(ctx, e.Database, snap.ID, e.Cluster); err != nil {
			return block("could not snapshot %s: %v", what, err)
		}
	}
	for waited := time.Duration(0); status != "available"; waited += cfg.SnapshotPoll {
		if waited >= cfg.SnapshotWait {
			return block("the snapshot %s of %s is still being made (big databases take a while). Run the same command again in a few minutes; it will use this snapshot.", snap.ID, what)
		}
		cfg.Sleep(cfg.SnapshotPoll)
		list, err := c.DBSnapshots(ctx, e.Database, snap.ID, e.Cluster)
		if err != nil {
			return block("could not check snapshot %s: %v", snap.ID, err)
		}
		if len(list) == 0 {
			return block("snapshot %s of %s disappeared", snap.ID, what)
		}
		status = list[0].Status
		if status == "failed" {
			return block("AWS could not make snapshot %s of %s", snap.ID, what)
		}
	}
	step.Snapshot = snap.ID
	r.Protection = append(r.Protection, fmt.Sprintf("made snapshot %s of %s", snap.ID, what))
	r.Notes = append(r.Notes, "The snapshot is kept until you delete it; AWS bills snapshot storage. Data written after the snapshot is not in it.")
	r.Undo = append(r.Undo, step)
	return true
}

// recentSnapshot finds a cloudundo snapshot of this database taken in the
// last reuseWindow, so a retried delete does not start another one.
func (r *Result) recentSnapshot(ctx context.Context, c awscli.Client, e effects.Effect, now time.Time) (awscli.DBSnapshot, error) {
	list, err := c.DBSnapshots(ctx, e.Database, "", e.Cluster)
	if err != nil {
		return awscli.DBSnapshot{}, err
	}
	prefix := "cloudundo-" + strings.ToLower(e.Database) + "-"
	var best awscli.DBSnapshot
	for _, s := range list {
		if !strings.HasPrefix(strings.ToLower(s.ID), prefix) || now.Sub(s.Created) > reuseWindow || s.Status == "failed" {
			continue
		}
		if best.ID == "" || s.Created.After(best.Created) {
			best = s
		}
	}
	return best, nil
}

// dbSnapshotName is a valid RDS snapshot identifier: letters, digits and
// single hyphens, starting with a letter, at most 255 characters.
func dbSnapshotName(db string, since time.Time) string {
	n := "cloudundo-" + strings.ToLower(db) + "-" + since.UTC().Format("20060102-150405")
	for strings.Contains(n, "--") {
		n = strings.ReplaceAll(n, "--", "-")
	}
	if len(n) > 255 {
		n = n[:255]
	}
	return strings.TrimRight(n, "-")
}

// cacheDelete allows an ElastiCache delete only when its data is kept or
// does not matter.
func (r *Result) cacheDelete(ctx context.Context, c awscli.Client, e effects.Effect) bool {
	if e.FinalSnapshot != "" {
		r.Notes = append(r.Notes, fmt.Sprintf("AWS will keep final snapshot %s of %s; create a new cache from it to get the data back (cloudundo does not restore caches).", e.FinalSnapshot, e.Cache))
		return true
	}
	if e.CacheKind != effects.CacheReplicationGroup {
		info, ok, err := c.DescribeCache(ctx, e.Cache, e.CacheKind == effects.CacheServerless)
		if err != nil {
			r.Blocked = append(r.Blocked, fmt.Sprintf("could not check cache %s: %v", e.Cache, err))
			return false
		}
		if !ok {
			r.Notes = append(r.Notes, fmt.Sprintf("Cache %s does not exist, so the command will fail on its own.", e.Cache))
			return true
		}
		if info.Engine == "memcached" {
			r.Notes = append(r.Notes, fmt.Sprintf("%s is a Memcached cache; Memcached keeps no data to restore.", e.Cache))
			return true
		}
		if info.ReplicationGroup != "" {
			r.Notes = append(r.Notes, fmt.Sprintf("%s is one node of replication group %s; the data stays in the group.", e.Cache, info.ReplicationGroup))
			return true
		}
	}
	flag := "--final-snapshot-identifier <name>"
	if e.CacheKind == effects.CacheServerless {
		flag = "--final-snapshot-name <name>"
	}
	r.Blocked = append(r.Blocked, fmt.Sprintf("%q deletes cache %s and its data with no snapshot. Add %s to keep a copy.", e.Command, e.Cache, flag))
	return false
}
