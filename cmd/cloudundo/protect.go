package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/cloudundo/cloudundo/internal/awscli"
	"github.com/cloudundo/cloudundo/internal/effects"
	"github.com/cloudundo/cloudundo/internal/journal"
	"github.com/cloudundo/cloudundo/internal/protect"
	"github.com/cloudundo/cloudundo/internal/undo"
)

// Swapped in tests.
var (
	newRunner = func() awscli.Runner { return awscli.Exec{Bin: os.Getenv("CLOUDUNDO_AWS_BIN")} }
	now       = time.Now
)

const exitBlock = 2

func clientFor(r awscli.Runner) func(awscli.Conn) awscli.Client {
	return func(c awscli.Conn) awscli.Client { return awscli.Client{R: r, C: c} }
}

func config() (protect.Config, error) {
	cfg := protect.Defaults()
	cfg.Now = now
	if v := os.Getenv("CLOUDUNDO_RETENTION_DAYS"); v != "" {
		n, err := strconv.Atoi(v)
		if err != nil || n < 1 {
			return cfg, fmt.Errorf("CLOUDUNDO_RETENTION_DAYS=%q is not a whole number of days", v)
		}
		cfg.RetentionDays = n
	}
	if v := os.Getenv("CLOUDUNDO_MAX_COPY_GB"); v != "" {
		n, err := strconv.ParseFloat(v, 64)
		if err != nil || n < 0 {
			return cfg, fmt.Errorf("CLOUDUNDO_MAX_COPY_GB=%q is not a number", v)
		}
		cfg.MaxCopyBytes = int64(n * (1 << 30))
	}
	return cfg, nil
}

// cmdProtect runs before the agent's command. Any failure, including its
// own, ends in exit 2 so the command does not run unprotected.
func cmdProtect(args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("protect", flag.ContinueOnError)
	fs.SetOutput(stderr)
	asJSON := fs.Bool("json", false, "print the result as JSON")
	if err := fs.Parse(args); err != nil {
		return exitBlock
	}
	line := strings.Join(fs.Args(), " ")
	if strings.TrimSpace(line) == "" {
		fmt.Fprintln(stderr, "cloudundo protect: no command given")
		return exitBlock
	}
	blockWith := func(msg string) int {
		fmt.Fprintln(stderr, "cloudundo: blocked:", msg)
		return exitBlock
	}

	effs := effects.Analyze(line)
	if len(effs) == 0 {
		if *asJSON {
			fmt.Fprintln(stdout, `{"allowed":true,"effects":[]}`)
		}
		return 0
	}
	cfg, err := config()
	if err != nil {
		return blockWith(err.Error())
	}
	dir, err := journal.DefaultDir()
	if err != nil {
		return blockWith("cannot find the journal folder: " + err.Error())
	}
	j, err := journal.Open(dir)
	if err != nil {
		return blockWith("cannot open the journal: " + err.Error())
	}
	cfg.DataDir = dir

	r := protect.Run(context.Background(), effs, clientFor(newRunner()), cfg)
	status := "protected"
	if !r.Allowed {
		status = "blocked"
	}
	entry, err := j.Append(journal.Entry{
		Time: now().UTC(), Command: line, Effects: effs,
		Protection: r.Protection, Notes: r.Notes, Blocked: r.Blocked, Undo: r.Undo, Status: status,
	})
	if err != nil {
		// Without a journal entry, undo could not find the protection.
		return blockWith("cannot write the journal: " + err.Error())
	}

	if *asJSON {
		out, _ := json.MarshalIndent(map[string]any{
			"id": entry.ID, "allowed": r.Allowed, "protection": r.Protection,
			"notes": r.Notes, "blocked": r.Blocked,
		}, "", "  ")
		fmt.Fprintln(stdout, string(out))
	}
	if !r.Allowed {
		fmt.Fprintf(stderr, "cloudundo: blocked #%d\n", entry.ID)
		for _, b := range r.Blocked {
			fmt.Fprintln(stderr, "  - "+b)
		}
		return exitBlock
	}
	if !*asJSON {
		fmt.Fprintf(stdout, "cloudundo: protected #%d\n", entry.ID)
		for _, p := range r.Protection {
			fmt.Fprintln(stdout, "  + "+p)
		}
		for _, n := range r.Notes {
			fmt.Fprintln(stdout, "  note: "+n)
		}
		if len(r.Undo) > 0 {
			fmt.Fprintf(stdout, "  Undo with: cloudundo undo %d\n", entry.ID)
		}
	}
	return 0
}

func cmdUndo(args []string, stdout, stderr io.Writer) int {
	yes := false
	var rest []string
	for _, a := range args {
		switch a {
		case "--yes", "-y":
			yes = true
		default:
			rest = append(rest, a)
		}
	}
	if len(rest) != 1 {
		fmt.Fprintln(stderr, "usage: cloudundo undo <id> [--yes]")
		return 2
	}
	id, err := strconv.Atoi(strings.TrimPrefix(rest[0], "#"))
	if err != nil {
		fmt.Fprintf(stderr, "cloudundo undo: %q is not a journal id\n", rest[0])
		return 2
	}
	j, err := openJournal()
	if err != nil {
		fmt.Fprintln(stderr, "cloudundo:", err)
		return 1
	}
	e, ok, err := j.Get(id)
	if err != nil {
		fmt.Fprintln(stderr, "cloudundo:", err)
		return 1
	}
	if !ok {
		fmt.Fprintf(stderr, "cloudundo undo: no entry #%d (see cloudundo log)\n", id)
		return 1
	}

	dir, err := journal.DefaultDir()
	if err != nil {
		fmt.Fprintln(stderr, "cloudundo:", err)
		return 1
	}
	ctx := context.Background()
	plan, err := undo.For(ctx, &e, clientFor(newRunner()), dir)
	if err != nil {
		fmt.Fprintf(stderr, "cloudundo undo #%d: %v\n", id, err)
		return 1
	}
	fmt.Fprintf(stdout, "#%d: %s\n", e.ID, e.Command)
	if len(plan.Actions) == 0 {
		fmt.Fprint(stdout, plan.Summary())
		if plan.Pending {
			fmt.Fprintln(stdout, "Still in progress; nothing to do right now.")
			return 0
		}
		fmt.Fprintln(stdout, "Nothing to restore.")
		return 0
	}
	fmt.Fprint(stdout, plan.Summary())
	if !yes {
		fmt.Fprintf(stdout, "Nothing changed yet. Run `cloudundo undo %d --yes` to do this.\n", id)
		return 0
	}

	n, err := plan.Apply(ctx)
	// Save progress either way: a started table restore must be remembered.
	if err != nil {
		if uerr := j.Update(e); uerr != nil {
			fmt.Fprintln(stderr, "cloudundo: could not save progress to the journal:", uerr)
		}
		fmt.Fprintf(stderr, "cloudundo undo #%d: stopped after %d of %d steps: %v\n", id, n, len(plan.Actions), err)
		fmt.Fprintln(stderr, "Nothing was deleted. Fix the problem and run the same command again.")
		return 1
	}
	if !plan.Pending {
		e.Status = "undone"
	}
	if err := j.Update(e); err != nil {
		fmt.Fprintln(stderr, "cloudundo: restored, but could not save to the journal:", err)
		if plan.Pending {
			return 1
		}
	}
	if plan.Pending {
		fmt.Fprintf(stdout, "Started (%d steps). Run `cloudundo undo %d --yes` again in a few minutes to finish.\n", n, id)
		return 0
	}
	fmt.Fprintf(stdout, "Restored (%d steps).\n", n)
	return 0
}
