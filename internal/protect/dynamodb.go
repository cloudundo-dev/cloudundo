package protect

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"time"

	"github.com/cloudundo-dev/cloudundo/internal/awscli"
	"github.com/cloudundo-dev/cloudundo/internal/effects"
	"github.com/cloudundo-dev/cloudundo/internal/journal"
)

// tableBackup makes an on-demand backup before a table is deleted, waits
// until it is usable, and records the settings a restore leaves out.
func (r *Result) tableBackup(ctx context.Context, c awscli.Client, e effects.Effect, since time.Time, cfg Config) bool {
	block := func(format string, a ...any) bool {
		r.Blocked = append(r.Blocked, fmt.Sprintf(format, a...))
		return false
	}
	info, ok, err := c.DescribeTable(ctx, e.Table)
	if err != nil {
		return block("could not check DynamoDB table %s: %v", e.Table, err)
	}
	if !ok {
		r.Notes = append(r.Notes, fmt.Sprintf("DynamoDB table %s does not exist, so the command will fail on its own.", e.Table))
		return true
	}
	if info.DeletionProtection {
		r.Notes = append(r.Notes, fmt.Sprintf("DynamoDB table %s has deletion protection on, so AWS will refuse to delete it.", e.Table))
		return true
	}

	// Settings first: reading them changes nothing.
	tc := &journal.TableConfig{StreamViewType: info.StreamViewType}
	if ttl, err := c.TTL(ctx, e.Table); err != nil {
		tc.Unread = append(tc.Unread, "TTL")
	} else {
		tc.TTLAttribute = ttl
	}
	if tags, err := c.Tags(ctx, info.Arn); err != nil {
		tc.Unread = append(tc.Unread, "tags")
	} else {
		tc.Tags = tags
	}
	if pitr, err := c.PITR(ctx, e.Table); err != nil {
		tc.Unread = append(tc.Unread, "point-in-time recovery")
	} else {
		tc.PITR = pitr
	}

	name := backupName(e.Table, since)
	arn, status, err := c.CreateBackup(ctx, e.Table, name)
	if err != nil {
		return block("could not back up DynamoDB table %s: %v", e.Table, err)
	}
	for waited := time.Duration(0); status != "AVAILABLE"; waited += time.Second {
		if waited >= cfg.BackupWait {
			return block("the backup of DynamoDB table %s is still being created. Try again in a minute; the backup %s is kept.", e.Table, arn)
		}
		cfg.Sleep(time.Second)
		if status, err = c.BackupStatus(ctx, arn); err != nil {
			return block("could not check the backup of DynamoDB table %s: %v", e.Table, err)
		}
	}

	r.Protection = append(r.Protection, fmt.Sprintf("made on-demand backup %s of DynamoDB table %s", name, e.Table))
	if len(tc.Unread) > 0 {
		r.Notes = append(r.Notes, fmt.Sprintf("Could not read the %v of %s, so undo will not bring those back.", tc.Unread, e.Table))
	}
	r.Notes = append(r.Notes, "The backup is kept until you delete it; on-demand backups are billed for storage.")
	r.Undo = append(r.Undo, journal.UndoStep{
		Kind: "ddb-table-backup", Table: e.Table, Since: since, BackupArn: arn,
		TableConfig: tc, Conn: e.Conn,
	})
	return true
}

var backupNameBad = regexp.MustCompile(`[^a-zA-Z0-9_.-]`)

func backupName(table string, since time.Time) string {
	n := "cloudundo-" + backupNameBad.ReplaceAllString(table, "_") + "-" + since.Format("20060102T150405Z")
	if len(n) > 255 {
		n = n[:255]
	}
	return n
}

// saveItems reads each item the command will delete or change and saves
// it next to the journal, so undo can put it back.
func (r *Result) saveItems(ctx context.Context, c awscli.Client, e effects.Effect, since time.Time, n int, cfg Config) bool {
	block := func(format string, a ...any) bool {
		r.Blocked = append(r.Blocked, fmt.Sprintf(format, a...))
		return false
	}
	if len(e.Items) > cfg.MaxItems {
		return block("%q changes %d items in %s; cloudundo saves at most %d one by one.", e.Command, len(e.Items), e.Table, cfg.MaxItems)
	}
	if cfg.DataDir == "" {
		return block("nowhere to save the items of %s (no journal folder)", e.Table)
	}
	info, ok, err := c.DescribeTable(ctx, e.Table)
	if err != nil {
		return block("could not check DynamoDB table %s: %v", e.Table, err)
	}
	if !ok {
		r.Notes = append(r.Notes, fmt.Sprintf("DynamoDB table %s does not exist, so the command will fail on its own.", e.Table))
		return true
	}

	var saved []journal.SavedItem
	existing := 0
	for _, raw := range e.Items {
		key := raw
		if e.FullItems {
			if key, err = keyFromItem(raw, info.KeyAttrs); err != nil {
				return block("could not find the key of an item for %s: %v", e.Table, err)
			}
		}
		old, err := c.GetItem(ctx, e.Table, key)
		if err != nil {
			return block("could not read an item from %s before changing it: %v", e.Table, err)
		}
		s := journal.SavedItem{Key: json.RawMessage(key), Item: json.RawMessage("null")}
		if old != "" {
			s.Item = json.RawMessage(old)
			existing++
		}
		saved = append(saved, s)
	}

	dir := filepath.Join(cfg.DataDir, "items")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return block("could not create %s: %v", dir, err)
	}
	file := filepath.Join(dir, fmt.Sprintf("%s-%s-%d.json", since.Format("20060102T150405.000000000Z"), backupNameBad.ReplaceAllString(e.Table, "_"), n))
	body, err := json.MarshalIndent(saved, "", "  ")
	if err != nil {
		return block("could not save items of %s: %v", e.Table, err)
	}
	if err := os.WriteFile(file, body, 0o600); err != nil {
		return block("could not save items of %s: %v", e.Table, err)
	}

	r.Protection = append(r.Protection, fmt.Sprintf("saved %d current item(s) of DynamoDB table %s before the change", existing, e.Table))
	r.Undo = append(r.Undo, journal.UndoStep{
		Kind: "ddb-items", Table: e.Table, Since: since, ItemsFile: file, Conn: e.Conn,
	})
	return true
}

// keyFromItem keeps only the key attributes of a full item.
func keyFromItem(item string, keyAttrs []string) (string, error) {
	var m map[string]json.RawMessage
	if err := json.Unmarshal([]byte(item), &m); err != nil {
		return "", err
	}
	k := map[string]json.RawMessage{}
	for _, a := range keyAttrs {
		v, ok := m[a]
		if !ok {
			return "", fmt.Errorf("the item has no %q attribute", a)
		}
		k[a] = v
	}
	b, err := json.Marshal(k)
	return string(b), err
}
