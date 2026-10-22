// Package cleanup lists and deletes the backups cloudundo kept, so they
// stop costing storage. Deleting a backup means its undo can no longer
// restore that data, so only the user runs it; the agent hooks block it.
package cleanup

import (
	"context"
	"errors"
	"fmt"
	"os"
	"strings"
	"time"

	"github.com/cloudundo/cloudundo/internal/awscli"
	"github.com/cloudundo/cloudundo/internal/journal"
)

// Backup is one backup cloudundo kept for an undo step.
type Backup struct {
	Entry  int
	Step   int // index in the entry's Undo
	What   string
	Kept   time.Time
	Undone bool // the entry was undone already
}

// List returns the backups still kept, oldest first. Old S3 versions are
// not listed: they expire on their own.
func List(entries []journal.Entry) []Backup {
	var out []Backup
	for _, e := range entries {
		for i, s := range e.Undo {
			if s.Cleaned != nil {
				continue
			}
			what := describe(s)
			if what == "" {
				continue
			}
			out = append(out, Backup{Entry: e.ID, Step: i, What: what, Kept: e.Time, Undone: e.Status == "undone"})
		}
	}
	return out
}

func describe(s journal.UndoStep) string {
	switch s.Kind {
	case "rds-snapshot":
		if s.Snapshot == "" {
			return ""
		}
		db := ""
		if s.DB != nil {
			db = " of " + s.DB.ID
		}
		return "RDS snapshot " + s.Snapshot + db
	case "ddb-table-backup":
		if s.BackupArn == "" {
			return ""
		}
		return "DynamoDB backup of " + s.Table + " (" + s.BackupArn + ")"
	case "s3-bucket-copy":
		if s.BackupBucket == "" {
			return ""
		}
		return "copy of s3://" + s.Bucket + " at s3://" + s.BackupBucket + "/" + s.BackupPrefix
	case "ddb-items":
		if s.ItemsFile == "" {
			return ""
		}
		return "saved items of DynamoDB table " + s.Table + " (" + s.ItemsFile + ")"
	}
	return ""
}

// ClientFor returns a CLI client for a Conn.
type ClientFor func(awscli.Conn) awscli.Client

// Delete removes one step's backup and marks it cleaned in e. A backup
// that is already gone counts as deleted.
func Delete(ctx context.Context, e *journal.Entry, step int, clientFor ClientFor, now time.Time) error {
	s := &e.Undo[step]
	c := clientFor(s.Conn)
	var err error
	switch s.Kind {
	case "rds-snapshot":
		cluster := s.DB != nil && s.DB.Cluster
		_, err = c.DeleteDBSnapshot(ctx, s.Snapshot, cluster)
	case "ddb-table-backup":
		_, err = c.DeleteBackup(ctx, s.BackupArn)
	case "s3-bucket-copy":
		bc := s.Conn
		bc.Region = s.Region
		err = deleteCopy(ctx, clientFor(bc), s.BackupBucket, s.BackupPrefix)
	case "ddb-items":
		err = os.Remove(s.ItemsFile)
		if errors.Is(err, os.ErrNotExist) {
			err = nil
		}
	default:
		return fmt.Errorf("nothing to clean up for a %s step", s.Kind)
	}
	if err != nil {
		return err
	}
	t := now.UTC()
	s.Cleaned = &t
	return nil
}

// deleteCopy removes every version under a bucket copy's prefix, so the
// copy stops costing storage (the backup bucket is versioned).
func deleteCopy(ctx context.Context, c awscli.Client, bucket, prefix string) error {
	if !strings.HasSuffix(prefix, "/") || strings.Count(prefix, "/") < 2 {
		// Prefixes are <bucket>/<time>/; anything else is not one of ours.
		return fmt.Errorf("refusing to delete s3://%s/%s: not a cloudundo copy", bucket, prefix)
	}
	vs, err := c.ListVersions(ctx, bucket, prefix)
	if awscli.Has(err, "NoSuchBucket") {
		return nil
	}
	if err != nil {
		return err
	}
	return c.DeleteVersions(ctx, bucket, vs)
}

// ParseAge reads "30d", "12h" or "90m".
func ParseAge(s string) (time.Duration, error) {
	if d, ok := strings.CutSuffix(s, "d"); ok {
		var n int
		if _, err := fmt.Sscanf(d, "%d", &n); err != nil || n < 0 || fmt.Sprint(n) != d {
			return 0, fmt.Errorf("%q is not an age such as 30d or 12h", s)
		}
		return time.Duration(n) * 24 * time.Hour, nil
	}
	d, err := time.ParseDuration(s)
	if err != nil || d < 0 {
		return 0, fmt.Errorf("%q is not an age such as 30d or 12h", s)
	}
	return d, nil
}
