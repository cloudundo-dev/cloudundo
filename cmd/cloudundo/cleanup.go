package main

import (
	"context"
	"flag"
	"fmt"
	"io"
	"strconv"
	"time"

	"github.com/cloudundo-dev/cloudundo/internal/cleanup"
	"github.com/cloudundo-dev/cloudundo/internal/journal"
)

// cmdCleanup lists the backups cloudundo kept, and with --yes deletes the
// chosen ones. The agent hooks block `cleanup --yes`: only the user
// decides which undos to give up.
func cmdCleanup(args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("cleanup", flag.ContinueOnError)
	fs.SetOutput(stderr)
	olderThan := fs.String("older-than", "", "only backups kept longer than this (30d, 12h)")
	all := fs.Bool("all", false, "every backup")
	yes := fs.Bool("yes", false, "delete them (without it, only list)")
	if err := fs.Parse(reorder(args)); err != nil {
		return 2
	}
	ids := map[int]bool{}
	for _, a := range fs.Args() {
		n, err := strconv.Atoi(a)
		if err != nil || n < 1 {
			fmt.Fprintf(stderr, "cloudundo cleanup: %q is not an entry number\n", a)
			return 2
		}
		ids[n] = true
	}
	var age time.Duration
	if *olderThan != "" {
		var err error
		if age, err = cleanup.ParseAge(*olderThan); err != nil {
			fmt.Fprintln(stderr, "cloudundo cleanup:", err)
			return 2
		}
	}
	chosen := *all || *olderThan != "" || len(ids) > 0
	if *yes && !chosen {
		fmt.Fprintln(stderr, "cloudundo cleanup: say which backups to delete: entry numbers, --older-than 30d, or --all")
		return 2
	}

	j, err := openJournal()
	if err != nil {
		fmt.Fprintln(stderr, "cloudundo:", err)
		return 1
	}
	entries, err := j.List()
	if err != nil {
		fmt.Fprintln(stderr, "cloudundo:", err)
		return 1
	}
	t := now()
	var picked []cleanup.Backup
	for _, b := range cleanup.List(entries) {
		switch {
		case len(ids) > 0 && !ids[b.Entry]:
		case *olderThan != "" && t.Sub(b.Kept) < age:
		default:
			picked = append(picked, b)
		}
	}
	if len(picked) == 0 {
		fmt.Fprintln(stdout, "No backups to clean up.")
		return 0
	}

	if !*yes {
		for _, b := range picked {
			state := "not undone"
			if b.Undone {
				state = "undone"
			}
			fmt.Fprintf(stdout, "#%-4d %s  %-10s %s\n", b.Entry, b.Kept.Local().Format("2006-01-02"), state, b.What)
		}
		fmt.Fprintf(stdout, "%d backup(s). Nothing deleted.", len(picked))
		if chosen {
			fmt.Fprintln(stdout, " Add --yes to delete these; their undo will no longer work.")
		} else {
			fmt.Fprintln(stdout, " To delete some: cloudundo cleanup --older-than 30d --yes (or entry numbers, or --all).")
		}
		return 0
	}

	byID := map[int]*journal.Entry{}
	for i := range entries {
		byID[entries[i].ID] = &entries[i]
	}
	failed := 0
	changed := map[int]bool{}
	for _, b := range picked {
		e := byID[b.Entry]
		gone, err := cleanup.Delete(context.Background(), e, b.Step, clientFor(newRunner()), t)
		if err != nil {
			failed++
			fmt.Fprintf(stderr, "#%d: could not delete %s: %v\n", b.Entry, b.What, err)
			continue
		}
		changed[b.Entry] = true
		if gone {
			fmt.Fprintf(stdout, "#%d: already gone: %s\n", b.Entry, b.What)
		} else {
			fmt.Fprintf(stdout, "#%d: deleted %s\n", b.Entry, b.What)
		}
	}
	for id := range changed {
		if err := j.Update(*byID[id]); err != nil {
			fmt.Fprintln(stderr, "cloudundo: deleted, but could not save to the journal:", err)
			return 1
		}
	}
	if failed > 0 {
		fmt.Fprintf(stderr, "%d backup(s) not deleted. If an IAM policy (cloudundo policy) protects them, run cleanup with credentials it does not cover.\n", failed)
		return 1
	}
	return 0
}

// reorder puts flags before entry numbers, so "cleanup 7 --yes" works.
func reorder(args []string) []string {
	var flags, rest []string
	for i := 0; i < len(args); i++ {
		a := args[i]
		switch {
		case a == "--older-than" || a == "-older-than":
			flags = append(flags, a)
			if i+1 < len(args) {
				flags = append(flags, args[i+1])
				i++
			}
		case len(a) > 1 && a[0] == '-':
			flags = append(flags, a)
		default:
			rest = append(rest, a)
		}
	}
	return append(flags, rest...)
}
