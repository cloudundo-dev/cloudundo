// Package journal keeps the local record of every command cloudundo saw,
// what it would change, and what was done to make it undoable. `log`
// reads it and `undo` will use it to find what to restore.
package journal

import (
	"bufio"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"time"

	"github.com/cloudundo/cloudundo/internal/awscli"
	"github.com/cloudundo/cloudundo/internal/effects"
)

// Entry is one recorded command.
type Entry struct {
	ID      int       `json:"id"`
	Time    time.Time `json:"time"`
	Command string    `json:"command"`
	Agent   string    `json:"agent,omitempty"`
	// Dir is the folder cloudundo read the command in (the agent's).
	Dir string `json:"dir,omitempty"`
	// AgentDir is the folder the agent's hook input named, if it differs.
	AgentDir string           `json:"agent_dir,omitempty"`
	Effects  []effects.Effect `json:"effects"`
	// Protection lists what cloudundo did before the command ran.
	Protection []string `json:"protection,omitempty"`
	// Notes are things the user should know (costs, undo windows).
	Notes []string `json:"notes,omitempty"`
	// Blocked lists why the command was stopped, if it was.
	Blocked []string   `json:"blocked,omitempty"`
	Undo    []UndoStep `json:"undo,omitempty"`
	// Status is "protected", "blocked", "unchanged" or "undone".
	Status string `json:"status"`
}

// UndoStep is what undo needs to reverse one effect.
type UndoStep struct {
	// Kind is "s3-versions" (restore from object versions) or
	// "s3-bucket-copy" (recreate a deleted bucket from a backup copy).
	Kind      string   `json:"kind"`
	Bucket    string   `json:"bucket"`
	Key       string   `json:"key,omitempty"`
	Keys      []string `json:"keys,omitempty"`
	Prefix    string   `json:"prefix,omitempty"`
	Recursive bool     `json:"recursive,omitempty"`
	// Since is the moment before the command ran; undo puts every key back
	// to how it was at this time.
	Since        time.Time   `json:"since"`
	Region       string      `json:"region,omitempty"`
	BackupBucket string      `json:"backup_bucket,omitempty"`
	BackupPrefix string      `json:"backup_prefix,omitempty"`
	Conn         awscli.Conn `json:"conn"`

	// DynamoDB: "ddb-items" keeps the items as they were in ItemsFile;
	// "ddb-table-backup" keeps an on-demand backup and the table settings
	// a restore does not bring back.
	Table       string       `json:"table,omitempty"`
	ItemsFile   string       `json:"items_file,omitempty"`
	BackupArn   string       `json:"backup_arn,omitempty"`
	TableConfig *TableConfig `json:"table_config,omitempty"`
	// RestoredID is the unique id (DynamoDB TableId, RDS resource id) of
	// what undo restored, so a later run can finish its settings and tell
	// it apart from a new resource with the same name (names and ARNs repeat).
	RestoredID      string `json:"restored_id,omitempty"`
	SettingsApplied bool   `json:"settings_applied,omitempty"`

	// RDS: "rds-snapshot" restores DB (an instance, or an Aurora cluster)
	// from Snapshot with the settings recorded before the delete.
	DB       *awscli.DBInfo `json:"db,omitempty"`
	Snapshot string         `json:"snapshot,omitempty"`

	// Cleaned is when `cloudundo cleanup` deleted this step's backup;
	// undo can no longer restore it.
	Cleaned *time.Time `json:"cleaned,omitempty"`
}

// TableConfig is the part of a DynamoDB table a backup restore leaves out.
type TableConfig struct {
	TTLAttribute       string       `json:"ttl_attribute,omitempty"`
	Tags               []awscli.Tag `json:"tags,omitempty"`
	PITR               bool         `json:"pitr,omitempty"`
	StreamViewType     string       `json:"stream_view_type,omitempty"`
	DeletionProtection bool         `json:"deletion_protection,omitempty"`
	// Unread lists settings that could not be read, so undo cannot restore them.
	Unread []string `json:"unread,omitempty"`
}

// SavedItem is one DynamoDB item as it was before a command. Item is
// null when there was no item with that key.
type SavedItem struct {
	Key  json.RawMessage `json:"key"`
	Item json.RawMessage `json:"item"`
}

// Journal is an append-only JSON-lines file.
type Journal struct {
	path string
}

// Open returns the journal in dir, creating dir if needed.
func Open(dir string) (*Journal, error) {
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return nil, fmt.Errorf("create %s: %w", dir, err)
	}
	return &Journal{path: filepath.Join(dir, "journal.jsonl")}, nil
}

// DefaultDir is $CLOUDUNDO_HOME, or ~/.cloudundo.
func DefaultDir() (string, error) {
	if d := os.Getenv("CLOUDUNDO_HOME"); d != "" {
		return d, nil
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(home, ".cloudundo"), nil
}

// Append assigns the next ID to e and writes it.
func (j *Journal) Append(e Entry) (Entry, error) {
	unlock, err := j.lock()
	if err != nil {
		return e, err
	}
	defer unlock()
	all, err := j.List()
	if err != nil {
		return e, err
	}
	e.ID = 1
	if n := len(all); n > 0 {
		e.ID = all[n-1].ID + 1
	}
	if e.Time.IsZero() {
		e.Time = time.Now().UTC()
	}
	line, err := json.Marshal(e)
	if err != nil {
		return e, err
	}
	f, err := os.OpenFile(j.path, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o600)
	if err != nil {
		return e, err
	}
	defer f.Close()
	if _, err := f.Write(append(line, '\n')); err != nil {
		return e, err
	}
	return e, f.Sync()
}

// List returns all entries, oldest first. A missing file is an empty journal.
func (j *Journal) List() ([]Entry, error) {
	f, err := os.Open(j.path)
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	defer f.Close()

	var out []Entry
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 0, 64*1024), 10*1024*1024)
	for n := 1; sc.Scan(); n++ {
		if len(sc.Bytes()) == 0 {
			continue
		}
		var e Entry
		if err := json.Unmarshal(sc.Bytes(), &e); err != nil {
			return out, fmt.Errorf("%s line %d: %w", j.path, n, err)
		}
		out = append(out, e)
	}
	return out, sc.Err()
}

// Get returns the entry with the given ID.
func (j *Journal) Get(id int) (Entry, bool, error) {
	all, err := j.List()
	if err != nil {
		return Entry{}, false, err
	}
	for _, e := range all {
		if e.ID == id {
			return e, true, nil
		}
	}
	return Entry{}, false, nil
}

// Update replaces the entry with the same ID. The file is rewritten
// through a temporary file so a crash never leaves it half written.
func (j *Journal) Update(e Entry) error {
	unlock, err := j.lock()
	if err != nil {
		return err
	}
	defer unlock()
	all, err := j.List()
	if err != nil {
		return err
	}
	found := false
	var buf []byte
	for _, old := range all {
		if old.ID == e.ID {
			old = e
			found = true
		}
		line, err := json.Marshal(old)
		if err != nil {
			return err
		}
		buf = append(append(buf, line...), '\n')
	}
	if !found {
		return fmt.Errorf("journal: no entry #%d", e.ID)
	}
	tmp := j.path + ".tmp"
	if err := os.WriteFile(tmp, buf, 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, j.path)
}
