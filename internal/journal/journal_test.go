package journal

import (
	"fmt"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/cloudundo-dev/cloudundo/internal/effects"
)

func TestAppendListGet(t *testing.T) {
	j, err := Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}

	got, err := j.List()
	if err != nil || len(got) != 0 {
		t.Fatalf("empty journal: got %v, %v", got, err)
	}

	e1, err := j.Append(Entry{Command: "aws s3 rm s3://b/k", Status: "seen",
		Effects: []effects.Effect{{Kind: effects.DeleteObjects, Bucket: "b", Key: "k"}}})
	if err != nil {
		t.Fatal(err)
	}
	e2, err := j.Append(Entry{Command: "aws s3 rb s3://b", Status: "seen"})
	if err != nil {
		t.Fatal(err)
	}
	if e1.ID != 1 || e2.ID != 2 {
		t.Fatalf("ids = %d, %d; want 1, 2", e1.ID, e2.ID)
	}

	all, err := j.List()
	if err != nil {
		t.Fatal(err)
	}
	if len(all) != 2 || all[0].Effects[0].Bucket != "b" {
		t.Fatalf("list = %+v", all)
	}

	e, ok, err := j.Get(2)
	if err != nil || !ok || e.Command != "aws s3 rb s3://b" {
		t.Fatalf("Get(2) = %+v, %v, %v", e, ok, err)
	}
	if _, ok, _ := j.Get(9); ok {
		t.Fatal("Get(9) found an entry")
	}
}

// Agents run commands in parallel, and undo can run while they do. Every
// entry must get its own number and none may be lost.
func TestConcurrentAppendsAndUpdates(t *testing.T) {
	j, err := Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	first, err := j.Append(Entry{Command: "first"})
	if err != nil {
		t.Fatal(err)
	}
	const n = 40
	var wg sync.WaitGroup
	errs := make(chan error, 2*n)
	for i := 0; i < n; i++ {
		wg.Add(2)
		go func(i int) {
			defer wg.Done()
			_, err := j.Append(Entry{Command: fmt.Sprintf("cmd %d", i)})
			errs <- err
		}(i)
		go func(i int) {
			defer wg.Done()
			e := first
			e.Status = fmt.Sprintf("update %d", i)
			errs <- j.Update(e)
		}(i)
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		if err != nil {
			t.Fatal(err)
		}
	}
	all, err := j.List()
	if err != nil {
		t.Fatal(err)
	}
	if len(all) != n+1 {
		t.Fatalf("%d entries, want %d: an update dropped appends", len(all), n+1)
	}
	seen := map[int]bool{}
	for _, e := range all {
		if seen[e.ID] {
			t.Fatalf("two entries got #%d", e.ID)
		}
		seen[e.ID] = true
	}
}

func TestStaleLockIsTakenOver(t *testing.T) {
	j, err := Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	lock := j.path + ".lock"
	if err := os.WriteFile(lock, []byte("12345\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	old := time.Now().Add(-time.Hour)
	os.Chtimes(lock, old, old)
	if _, err := j.Append(Entry{Command: "x"}); err != nil {
		t.Fatalf("a crashed process's lock blocked the journal: %v", err)
	}
	if _, err := os.Stat(lock); !os.IsNotExist(err) {
		t.Fatal("lock file left behind")
	}
}

func TestBusyLockTimesOutWithAClearMessage(t *testing.T) {
	j, err := Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	oldWait := lockWait
	lockWait = 50 * time.Millisecond
	t.Cleanup(func() { lockWait = oldWait })
	unlock, err := j.lock()
	if err != nil {
		t.Fatal(err)
	}
	defer unlock()
	_, err = j.Append(Entry{Command: "x"})
	if err == nil || !strings.Contains(err.Error(), "locked by another cloudundo") {
		t.Fatalf("err = %v", err)
	}
}
