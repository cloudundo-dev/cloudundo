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

	"github.com/cloudundo/cloudundo/internal/s3effects"
)

// Entry is one recorded command.
type Entry struct {
	ID      int                `json:"id"`
	Time    time.Time          `json:"time"`
	Command string             `json:"command"`
	Agent   string             `json:"agent,omitempty"`
	Effects []s3effects.Effect `json:"effects"`
	// Protection lists what cloudundo did before the command ran
	// (for example "versioning-enabled", "backup-copied"). Empty until
	// protections are implemented.
	Protection []string          `json:"protection,omitempty"`
	Undo       map[string]string `json:"undo,omitempty"`
	Status     string            `json:"status"`
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
