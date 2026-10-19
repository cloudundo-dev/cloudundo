// Package protect makes the S3 changes of a command recoverable before
// the command runs, and records what undo will need.
//
// The rule is fail closed: if any effect of the command cannot be
// protected, nothing is changed and the command is blocked.
package protect

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"github.com/cloudundo/cloudundo/internal/awscli"
	"github.com/cloudundo/cloudundo/internal/effects"
	"github.com/cloudundo/cloudundo/internal/journal"
)

// Config tunes protection.
type Config struct {
	// RetentionDays is how long old versions are kept on buckets where
	// cloudundo turned versioning on. This is the undo window there.
	RetentionDays int
	// MaxCopyBytes is the largest bucket copied before a bucket delete.
	MaxCopyBytes int64
	Now          func() time.Time
	// DataDir is where saved DynamoDB items are written (the journal dir).
	DataDir string
	// BackupWait is how long to wait for a DynamoDB backup to be ready.
	BackupWait time.Duration
	Sleep      func(time.Duration)
	// MaxItems is the most DynamoDB items saved one by one per command.
	MaxItems int
	// SnapshotWait is how long to wait for an RDS snapshot before
	// blocking (the agent can retry; the snapshot is reused).
	SnapshotWait time.Duration
	SnapshotPoll time.Duration
	// Planner reads what a terraform command would delete.
	Planner Planner
}

// Defaults: 7 days, 5 GiB, wait up to 60 s for a table backup, 100 items.
func Defaults() Config {
	return Config{RetentionDays: 7, MaxCopyBytes: 5 << 30, Now: time.Now,
		BackupWait: 60 * time.Second, Sleep: time.Sleep, MaxItems: 100,
		SnapshotWait: 7 * time.Minute, SnapshotPoll: 10 * time.Second, Planner: ExecPlanner}
}

// Result is the outcome for one command.
type Result struct {
	Allowed    bool
	Protection []string
	Notes      []string
	Blocked    []string
	Undo       []journal.UndoStep
}

// ClientFor returns a CLI client for a Conn.
type ClientFor func(awscli.Conn) awscli.Client

// ExpiryRuleID names the lifecycle rule cloudundo adds.
const ExpiryRuleID = "cloudundo-expire-old-versions"

// BackupBucketPrefix starts the name of every backup bucket.
const BackupBucketPrefix = "cloudundo-backup-"

// Run protects every effect, or blocks the command.
func Run(ctx context.Context, effs []effects.Effect, clientFor ClientFor, cfg Config) Result {
	var r Result
	since := cfg.Now().UTC()

	// 1. Refuse what cannot be protected before changing anything.
	r.check(effs)
	if len(r.Blocked) > 0 {
		return r
	}

	// 2. Read what terraform commands would delete, and check that too.
	var all []effects.Effect
	for _, e := range effs {
		if e.Kind != effects.TerraformChange {
			all = append(all, e)
			continue
		}
		derived, ok := r.terraformPlan(ctx, e, clientFor, cfg)
		if !ok {
			return r.fail()
		}
		r.check(derived)
		if len(r.Blocked) > 0 {
			return r.fail()
		}
		all = append(all, derived...)
	}

	// 3. Protect, once per bucket and account.
	versioned := map[string]bool{}
	saved := 0
	for _, e := range all {
		c := clientFor(e.Conn)
		ok := true
		switch e.Kind {
		case effects.DeleteObjects, effects.OverwriteObjects:
			key := e.Conn.Key() + "|" + e.Bucket
			if !versioned[key] {
				versioned[key] = true
				ok = r.versioning(ctx, c, e.Bucket, cfg)
			}
			if ok {
				r.Undo = append(r.Undo, journal.UndoStep{
					Kind: "s3-versions", Bucket: e.Bucket, Key: e.Key, Keys: e.Keys,
					Prefix: e.Prefix, Recursive: e.Recursive, Since: since, Conn: e.Conn,
				})
			}
		case effects.DeleteBucket:
			ok = r.bucketCopy(ctx, c, e, since, cfg)
		case effects.DeleteTable:
			ok = r.tableBackup(ctx, c, e, since, cfg)
		case effects.DeleteItems, effects.OverwriteItems:
			saved++
			ok = r.saveItems(ctx, c, e, since, saved, cfg)
		case effects.DeleteDatabase:
			ok = r.dbSnapshot(ctx, c, e, since, cfg)
		case effects.DeleteCache:
			ok = r.cacheDelete(ctx, c, e)
		default:
			// Fail closed: a kind this version cannot protect is blocked.
			r.Blocked = append(r.Blocked, fmt.Sprintf("cloudundo cannot protect %q (%s) yet.", e.Command, e.Kind))
			ok = false
		}
		if !ok {
			return r.fail()
		}
	}
	r.Allowed = true
	return r
}

// check blocks effects that cannot be protected at all.
func (r *Result) check(effs []effects.Effect) {
	for _, e := range effs {
		cmd := e.Command
		if e.Via != "" {
			cmd += " → " + e.Via
		}
		switch {
		case strings.HasPrefix(e.Bucket, BackupBucketPrefix) && e.Kind != effects.OverwriteObjects:
			r.Blocked = append(r.Blocked, fmt.Sprintf(
				"%q would change s3://%s, where cloudundo keeps its backups. Remove backups yourself, not through an agent.",
				cmd, e.Bucket))
		case e.Uncertain:
			r.Blocked = append(r.Blocked, fmt.Sprintf(
				"cannot tell exactly what %q would change (%s). Use explicit names, or run it yourself.",
				cmd, target(e)))
		case e.Kind == effects.EraseCacheData:
			r.Blocked = append(r.Blocked, fmt.Sprintf("%q: %s. Run it yourself if you mean it.", cmd, e.Detail))
		case e.Kind == effects.WeakenProtection:
			r.Blocked = append(r.Blocked, fmt.Sprintf(
				"%q %s, which makes data loss on %s harder or impossible to undo. Run it yourself if you mean it.",
				cmd, e.Detail, target(e)))
		}
	}
}

// terraformPlan asks terraform what the command would delete.
func (r *Result) terraformPlan(ctx context.Context, e effects.Effect, clientFor ClientFor, cfg Config) ([]effects.Effect, bool) {
	if cfg.Planner == nil {
		r.Blocked = append(r.Blocked, "no way to read terraform plans is configured")
		return nil, false
	}
	body, err := cfg.Planner(ctx, *e.TF, e.Conn)
	if err != nil {
		r.Blocked = append(r.Blocked, fmt.Sprintf("could not see what %q would delete: %v", e.Command, err))
		return nil, false
	}
	derived, others, err := fromPlan(body, e)
	if err != nil {
		r.Blocked = append(r.Blocked, err.Error())
		return nil, false
	}
	if len(derived) > 0 {
		if err := checkAccounts(ctx, body, clientFor(e.Conn)); err != nil {
			r.Blocked = append(r.Blocked, fmt.Sprintf("%q: %v", e.Command, err))
			return nil, false
		}
	}
	if len(others) > 0 {
		r.Notes = append(r.Notes, fmt.Sprintf("%q also deletes %d resource(s) that hold no data (%s); cloudundo restores data, not infrastructure.",
			e.Command, len(others), shortList(others, 4)))
	}
	if len(derived) == 0 {
		r.Notes = append(r.Notes, fmt.Sprintf("%q deletes no data cloudundo knows (S3, DynamoDB, RDS, ElastiCache).", e.Command))
	}
	return derived, true
}

func shortList(s []string, n int) string {
	if len(s) <= n {
		return strings.Join(s, ", ")
	}
	return strings.Join(s[:n], ", ") + fmt.Sprintf(" and %d more", len(s)-n)
}

// fail turns a partial run into a block. Protections already applied
// (versioning on, a backup copy) are harmless and stay in place.
func (r Result) fail() Result {
	r.Allowed = false
	r.Undo = nil
	return r
}

// versioning makes object deletes and overwrites in bucket recoverable.
func (r *Result) versioning(ctx context.Context, c awscli.Client, bucket string, cfg Config) bool {
	status, err := c.VersioningStatus(ctx, bucket)
	if awscli.Has(err, "NoSuchBucket") {
		r.Notes = append(r.Notes, fmt.Sprintf("s3://%s does not exist, so the command will fail on its own.", bucket))
		return true
	}
	if err != nil {
		r.Blocked = append(r.Blocked, fmt.Sprintf("could not check versioning on s3://%s: %v", bucket, err))
		return false
	}

	switch status {
	case "Enabled":
		r.Protection = append(r.Protection, fmt.Sprintf("versioning was already on for s3://%s", bucket))
		return true
	case "Suspended":
		if err := c.EnableVersioning(ctx, bucket); err != nil {
			r.Blocked = append(r.Blocked, fmt.Sprintf("could not turn versioning back on for s3://%s: %v", bucket, err))
			return false
		}
		r.Protection = append(r.Protection, fmt.Sprintf("turned versioning back on for s3://%s", bucket))
		r.Notes = append(r.Notes, fmt.Sprintf(
			"s3://%s already had old versions from before, so no expiry rule was added; old versions are kept until you remove them.", bucket))
		return true
	}

	// Never versioned: turn it on, then make old versions expire so the
	// bucket does not grow forever. Every old version from now on is one
	// cloudundo created, so expiring them takes nothing the owner kept.
	if err := c.EnableVersioning(ctx, bucket); err != nil {
		r.Blocked = append(r.Blocked, fmt.Sprintf("could not turn on versioning for s3://%s: %v", bucket, err))
		return false
	}
	r.Protection = append(r.Protection, fmt.Sprintf(
		"turned on versioning for s3://%s (it can be suspended later, not removed)", bucket))
	r.addExpiry(ctx, c, bucket, cfg)
	return true
}

// addExpiry adds the expiry rule next to the bucket's existing rules.
// Failure here does not block: the data is already protected, it only
// costs more storage.
func (r *Result) addExpiry(ctx context.Context, c awscli.Client, bucket string, cfg Config) {
	warn := func(err error) {
		r.Notes = append(r.Notes, fmt.Sprintf(
			"could not add the %d-day expiry rule to s3://%s (%v); old versions are kept until you remove them.",
			cfg.RetentionDays, bucket, err))
	}
	lc, _, err := c.GetLifecycle(ctx, bucket)
	if err != nil {
		warn(err)
		return
	}
	for _, raw := range lc.Rules {
		var rule struct {
			ID                          string
			Status                      string
			NoncurrentVersionExpiration json.RawMessage
		}
		if json.Unmarshal(raw, &rule) != nil {
			continue
		}
		if rule.ID == ExpiryRuleID {
			r.Notes = append(r.Notes, fmt.Sprintf("Undo works on s3://%s for %d days.", bucket, cfg.RetentionDays))
			return
		}
		if rule.Status == "Enabled" && len(rule.NoncurrentVersionExpiration) > 0 {
			r.Notes = append(r.Notes, fmt.Sprintf(
				"s3://%s already has a rule that expires old versions; undo works for as long as that rule keeps them.", bucket))
			return
		}
	}
	ours, _ := json.Marshal(map[string]any{
		"ID":                          ExpiryRuleID,
		"Status":                      "Enabled",
		"Filter":                      map[string]string{"Prefix": ""},
		"NoncurrentVersionExpiration": map[string]int{"NoncurrentDays": cfg.RetentionDays},
	})
	lc.Rules = append(lc.Rules, ours)
	if err := c.PutLifecycle(ctx, bucket, lc); err != nil {
		warn(err)
		return
	}
	r.Protection = append(r.Protection, fmt.Sprintf("old versions in s3://%s now expire after %d days", bucket, cfg.RetentionDays))
	r.Notes = append(r.Notes, fmt.Sprintf("Undo works on s3://%s for %d days.", bucket, cfg.RetentionDays))
}

// bucketCopy copies a bucket before it is deleted, and checks the copy.
func (r *Result) bucketCopy(ctx context.Context, c awscli.Client, e effects.Effect, since time.Time, cfg Config) bool {
	block := func(format string, a ...any) bool {
		r.Blocked = append(r.Blocked, fmt.Sprintf(format, a...))
		return false
	}

	exists, err := c.BucketExists(ctx, e.Bucket)
	if err != nil {
		return block("could not check s3://%s: %v", e.Bucket, err)
	}
	if !exists {
		r.Notes = append(r.Notes, fmt.Sprintf("s3://%s does not exist, so the command will fail on its own.", e.Bucket))
		return true
	}
	count, size, err := c.BucketSummary(ctx, e.Bucket, "")
	if err != nil {
		return block("could not measure s3://%s: %v", e.Bucket, err)
	}
	if count == 0 {
		r.Notes = append(r.Notes, fmt.Sprintf("s3://%s is empty; nothing to back up.", e.Bucket))
		return true
	}
	if !e.Force {
		r.Notes = append(r.Notes, fmt.Sprintf(
			"s3://%s still holds %d objects, so AWS will refuse to delete it.", e.Bucket, count))
		return true
	}
	if size > cfg.MaxCopyBytes {
		return block("s3://%s holds %s, more than the %s cloudundo copies before a bucket delete. Back it up yourself, or raise CLOUDUNDO_MAX_COPY_GB.",
			e.Bucket, human(size), human(cfg.MaxCopyBytes))
	}

	region, err := c.BucketRegion(ctx, e.Bucket)
	if err != nil {
		return block("could not find the region of s3://%s: %v", e.Bucket, err)
	}
	account, err := c.AccountID(ctx)
	if err != nil {
		return block("could not read the AWS account: %v", err)
	}
	rc := c
	rc.C.Region = region
	backup := fmt.Sprintf("%s%s-%s", BackupBucketPrefix, account, region)
	ok, err := rc.BucketExists(ctx, backup)
	if err != nil {
		return block("could not check the backup bucket s3://%s: %v", backup, err)
	}
	if !ok {
		if err := rc.CreateBucket(ctx, backup, region); err != nil {
			return block("could not create the backup bucket s3://%s: %v", backup, err)
		}
		r.Protection = append(r.Protection, fmt.Sprintf("created private backup bucket s3://%s", backup))
	}
	// A versioned backup bucket keeps old copies even if something writes
	// over them. Best effort: a backup is still worth making without it.
	if st, err := rc.VersioningStatus(ctx, backup); err != nil || st != "Enabled" {
		if err := rc.EnableVersioning(ctx, backup); err != nil {
			r.Notes = append(r.Notes, fmt.Sprintf("Could not turn on versioning for the backup bucket s3://%s, so backups written over there cannot be recovered.", backup))
		}
	}

	prefix := fmt.Sprintf("%s/%s/", e.Bucket, since.Format("20060102T150405Z"))
	if err := rc.Sync(ctx, "s3://"+e.Bucket, "s3://"+backup+"/"+prefix); err != nil {
		return block("copying s3://%s to the backup failed: %v", e.Bucket, err)
	}
	gotCount, gotSize, err := rc.BucketSummary(ctx, backup, prefix)
	if err != nil {
		return block("could not verify the backup of s3://%s: %v", e.Bucket, err)
	}
	if gotCount != count || gotSize != size {
		return block("the backup of s3://%s is incomplete (%d of %d objects), so the delete was stopped.", e.Bucket, gotCount, count)
	}

	r.Protection = append(r.Protection, fmt.Sprintf("copied %d objects (%s) from s3://%s to s3://%s/%s",
		count, human(size), e.Bucket, backup, prefix))
	r.Notes = append(r.Notes, "The backup copy is kept until you delete it.")
	r.Undo = append(r.Undo, journal.UndoStep{
		Kind: "s3-bucket-copy", Bucket: e.Bucket, Since: since, Region: region,
		BackupBucket: backup, BackupPrefix: prefix, Conn: e.Conn,
	})
	return true
}

func target(e effects.Effect) string {
	switch e.Service {
	case "rds":
		if !e.Uncertain && e.Database != "" && e.Database != "?" {
			return "RDS " + e.Database
		}
		return "an RDS database"
	case "elasticache":
		if !e.Uncertain && e.Cache != "" {
			return "cache " + e.Cache
		}
		return "an ElastiCache cache"
	}
	if e.Service == "dynamodb" {
		if e.Table == "" || e.Table == "?" {
			return "a DynamoDB table"
		}
		return "DynamoDB table " + e.Table
	}
	b := e.Bucket
	if b == "" {
		b = "<unknown bucket>"
	}
	switch {
	case e.Key != "":
		return "s3://" + b + "/" + e.Key
	case e.Recursive:
		return "s3://" + b + "/" + e.Prefix + "*"
	}
	if e.Detail != "" {
		return "s3://" + b + ", " + e.Detail
	}
	return "s3://" + b
}

func human(n int64) string {
	const unit = 1024
	if n < unit {
		return fmt.Sprintf("%d B", n)
	}
	div, exp := int64(unit), 0
	for m := n / unit; m >= unit; m /= unit {
		div *= unit
		exp++
	}
	return fmt.Sprintf("%.1f %sB", float64(n)/float64(div), strings.Split("KMGTPE", "")[exp])
}
