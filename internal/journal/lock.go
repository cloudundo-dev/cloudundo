package journal

import (
	"errors"
	"fmt"
	"os"
	"time"
)

// The journal is shared by every agent session and terminal on the
// machine: an agent can run several commands at once, and the user may
// run undo while it works. Every read-modify-write of the file happens
// under a lock file, so two commands never get the same number and an
// undo never drops an entry written while it ran.
//
// The lock is a file created with O_EXCL, which works the same on every
// system. It is held for milliseconds (never during AWS calls), so a lock
// file older than staleLock was left by a crashed process and is removed.
var (
	lockWait  = 15 * time.Second
	lockPoll  = 5 * time.Millisecond
	staleLock = 30 * time.Second
)

func (j *Journal) lock() (unlock func(), err error) {
	path := j.path + ".lock"
	deadline := time.Now().Add(lockWait)
	for {
		f, err := os.OpenFile(path, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o600)
		if err == nil {
			fmt.Fprintf(f, "%d\n", os.Getpid())
			f.Close()
			return func() { os.Remove(path) }, nil
		}
		if !errors.Is(err, os.ErrExist) {
			return nil, fmt.Errorf("lock the journal: %w", err)
		}
		if st, serr := os.Stat(path); serr == nil && time.Since(st.ModTime()) > staleLock {
			os.Remove(path) // left by a process that crashed while holding it
			continue
		}
		if time.Now().After(deadline) {
			return nil, fmt.Errorf("the journal is locked by another cloudundo (%s); if no cloudundo is running, delete that file", path)
		}
		time.Sleep(lockPoll)
	}
}
