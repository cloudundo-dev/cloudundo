package undo

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"time"

	"github.com/cloudundo/cloudundo/internal/awscli"
	"github.com/cloudundo/cloudundo/internal/journal"
)

// planItems puts saved DynamoDB items back. DynamoDB keeps no history, so
// the items' current values are saved to a file first: nothing is lost,
// and the undo itself can be reversed by hand.
func planItems(ctx context.Context, c awscli.Client, s journal.UndoStep, p *Plan, dataDir string, entryID int) error {
	body, err := os.ReadFile(s.ItemsFile)
	if err != nil {
		return fmt.Errorf("the saved items of %s are missing: %w", s.Table, err)
	}
	var saved []journal.SavedItem
	if err := json.Unmarshal(body, &saved); err != nil {
		return fmt.Errorf("could not read the saved items of %s: %w", s.Table, err)
	}
	if _, ok, err := c.DescribeTable(ctx, s.Table); err != nil {
		return err
	} else if !ok {
		return fmt.Errorf("DynamoDB table %s does not exist; if it was deleted, undo that command first", s.Table)
	}

	type restore struct {
		key, old, cur string
	}
	var todo []restore
	for _, it := range saved {
		key := compactRaw(it.Key)
		cur, err := c.GetItem(ctx, s.Table, key)
		if err != nil {
			return err
		}
		if string(it.Item) == "null" || len(it.Item) == 0 {
			if cur != "" {
				p.Notes = append(p.Notes, fmt.Sprintf("item %s in %s did not exist before, so it is left in place.", key, s.Table))
			}
			continue
		}
		old := compactRaw(it.Item)
		if sameJSON(old, cur) {
			continue
		}
		todo = append(todo, restore{key, old, cur})
	}
	if len(todo) == 0 {
		return nil
	}

	var current []journal.SavedItem
	for _, r := range todo {
		if r.cur != "" {
			current = append(current, journal.SavedItem{Key: json.RawMessage(r.key), Item: json.RawMessage(r.cur)})
		}
	}
	if len(current) > 0 {
		if dataDir == "" {
			return fmt.Errorf("nowhere to save the current items of %s before restoring them", s.Table)
		}
		file := filepath.Join(dataDir, "items", fmt.Sprintf("undo-%d-%s-%s.json",
			entryID, backupSafe(s.Table), time.Now().UTC().Format("20060102T150405Z")))
		p.Notes = append(p.Notes, fmt.Sprintf("The current values of %d item(s) are saved to %s first.", len(current), file))
		p.Actions = append(p.Actions, Action{
			Desc: fmt.Sprintf("save the current values of %d item(s) in %s", len(current), s.Table),
			do: func(context.Context) error {
				if err := os.MkdirAll(filepath.Dir(file), 0o700); err != nil {
					return err
				}
				b, err := json.MarshalIndent(current, "", "  ")
				if err != nil {
					return err
				}
				return os.WriteFile(file, b, 0o600)
			},
		})
	}
	for _, r := range todo {
		r := r
		verb := "put back"
		if r.cur != "" {
			verb = "restore"
		}
		p.Actions = append(p.Actions, Action{
			Desc: fmt.Sprintf("%s item %s in %s", verb, r.key, s.Table),
			do:   func(ctx context.Context) error { return c.PutItem(ctx, s.Table, r.old) },
		})
	}
	return nil
}

// planTable restores a deleted table from its backup, then, once the
// table is ACTIVE, puts back the settings a restore leaves out. It can
// take several undo runs for a large table.
func planTable(ctx context.Context, c awscli.Client, s *journal.UndoStep, p *Plan) error {
	info, exists, err := c.DescribeTable(ctx, s.Table)
	if err != nil {
		return err
	}

	switch {
	case !exists:
		p.Actions = append(p.Actions, Action{
			Desc: fmt.Sprintf("restore DynamoDB table %s from backup %s", s.Table, s.BackupArn),
			do: func(ctx context.Context) error {
				arn, err := c.RestoreTable(ctx, restoreName(s.Table), s.BackupArn)
				if err != nil {
					return err
				}
				s.RestoredID = arn
				s.SettingsApplied = false
				return nil
			},
		})
		p.Notes = append(p.Notes, fmt.Sprintf(
			"Restoring %s can take a few minutes, or longer for a big table. Run the same undo again afterwards to put back its TTL, tags, streams and backups setting.", s.Table))
		p.Pending = true
		return nil

	case s.RestoredID == "" && info.Status == "DELETING":
		p.Notes = append(p.Notes, fmt.Sprintf("%s is still being deleted. Run the same undo again in a minute.", s.Table))
		p.Pending = true
		return nil

	case s.RestoredID == "":
		p.Notes = append(p.Notes, fmt.Sprintf(
			"DynamoDB table %s still exists, so there is nothing to restore. The backup %s is kept.", s.Table, s.BackupArn))
		return nil

	case info.ID != s.RestoredID:
		p.Notes = append(p.Notes, fmt.Sprintf(
			"A different table named %s exists now, so it is left alone. The backup %s is kept.", s.Table, s.BackupArn))
		return nil

	case s.SettingsApplied:
		return nil

	case info.Status != "ACTIVE":
		p.Notes = append(p.Notes, fmt.Sprintf("%s is still being restored (%s). Run the same undo again in a few minutes.", s.Table, info.Status))
		p.Pending = true
		return nil
	}

	// The restored table is ready: put back its settings.
	tc := s.TableConfig
	if tc == nil {
		tc = &journal.TableConfig{}
	}
	if tc.TTLAttribute != "" {
		cur, err := c.TTL(ctx, s.Table)
		if err != nil {
			return err
		}
		if cur != tc.TTLAttribute {
			attr := tc.TTLAttribute
			p.Actions = append(p.Actions, Action{
				Desc: fmt.Sprintf("turn TTL back on (%s) for %s", attr, s.Table),
				do:   func(ctx context.Context) error { return c.SetTTL(ctx, s.Table, attr) },
			})
		}
	}
	if tc.PITR {
		on, err := c.PITR(ctx, s.Table)
		if err != nil {
			return err
		}
		if !on {
			p.Actions = append(p.Actions, Action{
				Desc: "turn point-in-time recovery back on for " + s.Table,
				do:   func(ctx context.Context) error { return c.EnablePITR(ctx, s.Table) },
			})
		}
	}
	if tc.StreamViewType != "" && info.StreamViewType == "" {
		view := tc.StreamViewType
		p.Actions = append(p.Actions, Action{
			Desc: fmt.Sprintf("turn the stream back on (%s) for %s", view, s.Table),
			do:   func(ctx context.Context) error { return c.EnableStream(ctx, s.Table, view) },
		})
	}
	if len(tc.Tags) > 0 {
		cur, err := c.Tags(ctx, info.Arn)
		if err != nil {
			return err
		}
		if !reflect.DeepEqual(tagMap(cur), tagMap(tc.Tags)) {
			tags := tc.Tags
			p.Actions = append(p.Actions, Action{
				Desc: fmt.Sprintf("put back %d tag(s) on %s", len(tags), s.Table),
				do:   func(ctx context.Context) error { return c.TagResource(ctx, info.Arn, tags) },
			})
		}
	}
	if tc.DeletionProtection {
		p.Actions = append(p.Actions, Action{
			Desc: "turn deletion protection back on for " + s.Table,
			do:   func(ctx context.Context) error { return c.EnableDeletionProtection(ctx, s.Table) },
		})
	}
	if len(tc.Unread) > 0 {
		p.Notes = append(p.Notes, fmt.Sprintf("The %v of %s could not be read before the delete, so they are not put back.", tc.Unread, s.Table))
	}
	p.Notes = append(p.Notes, fmt.Sprintf("Auto scaling, alarms and IAM policies of %s are not restored.", s.Table))
	p.Actions = append(p.Actions, Action{
		Desc: "mark " + s.Table + " as fully restored",
		do: func(context.Context) error {
			s.SettingsApplied = true
			return nil
		},
	})
	return nil
}

func tagMap(tags []awscli.Tag) map[string]string {
	m := map[string]string{}
	for _, t := range tags {
		m[t.Key] = t.Value
	}
	return m
}

func compactRaw(b json.RawMessage) string {
	var v any
	if json.Unmarshal(b, &v) != nil {
		return string(b)
	}
	out, _ := json.Marshal(v)
	return string(out)
}

func sameJSON(a, b string) bool {
	if a == "" || b == "" {
		return a == b
	}
	var x, y any
	if json.Unmarshal([]byte(a), &x) != nil || json.Unmarshal([]byte(b), &y) != nil {
		return a == b
	}
	return reflect.DeepEqual(x, y)
}

func backupSafe(s string) string {
	out := []rune{}
	for _, r := range s {
		if (r >= 'a' && r <= 'z') || (r >= 'A' && r <= 'Z') || (r >= '0' && r <= '9') || r == '-' || r == '_' || r == '.' {
			out = append(out, r)
		} else {
			out = append(out, '_')
		}
	}
	return string(out)
}

// restoreName is the table name to restore into. Commands may name a
// table by ARN, but a restore needs the bare name.
func restoreName(table string) string {
	if strings.HasPrefix(table, "arn:") {
		if i := strings.LastIndex(table, "table/"); i >= 0 {
			return table[i+len("table/"):]
		}
	}
	return table
}
