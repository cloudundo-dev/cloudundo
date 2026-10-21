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
	newRunner = func() awscli.Runner {
		return awscli.Exec{Bin: os.Getenv("CLOUDUNDO_AWS_BIN"), Progress: os.Stderr}
	}
	now    = time.Now
	getenv = os.Getenv
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

// outcome is what protection decided for one command line.
type outcome struct {
	Allowed    bool
	ID         int // journal entry, 0 when nothing was recorded
	Effects    int
	Protection []string
	Notes      []string
	Blocked    []string
	CanUndo    bool
}

// message is a short human summary of an outcome.
func (o outcome) message() string {
	var b strings.Builder
	if !o.Allowed {
		if o.ID > 0 {
			fmt.Fprintf(&b, "cloudundo: blocked #%d\n", o.ID)
		} else {
			b.WriteString("cloudundo: blocked\n")
		}
		for _, x := range o.Blocked {
			b.WriteString("  - " + x + "\n")
		}
		return b.String()
	}
	fmt.Fprintf(&b, "cloudundo: protected #%d\n", o.ID)
	for _, p := range o.Protection {
		b.WriteString("  + " + p + "\n")
	}
	for _, n := range o.Notes {
		b.WriteString("  note: " + n + "\n")
	}
	if o.CanUndo {
		fmt.Fprintf(&b, "  Undo with: cloudundo undo %d\n", o.ID)
	}
	return b.String()
}

func blocked(msg string) outcome {
	return outcome{Blocked: []string{msg}}
}

// decide protects one command line and records it. Any failure,
// including a panic, ends in a block so the command does not run
// unprotected.
func decide(line, agent string) (o outcome) {
	defer func() {
		if r := recover(); r != nil {
			o = blocked(fmt.Sprintf("internal error: %v", r))
		}
	}()
	if strings.TrimSpace(line) == "" {
		return outcome{Allowed: true}
	}
	effs := effects.Analyze(line)
	if len(effs) == 0 {
		return outcome{Allowed: true}
	}
	for i := range effs {
		effs[i].Conn.FillEnv(getenv)
	}
	cfg, err := config()
	if err != nil {
		return blocked(err.Error())
	}
	dir, err := journal.DefaultDir()
	if err != nil {
		return blocked("cannot find the journal folder: " + err.Error())
	}
	j, err := journal.Open(dir)
	if err != nil {
		return blocked("cannot open the journal: " + err.Error())
	}
	cfg.DataDir = dir

	r := protect.Run(context.Background(), effs, clientFor(newRunner()), cfg)
	status := "protected"
	if !r.Allowed {
		status = "blocked"
	}
	entry, err := j.Append(journal.Entry{
		Time: now().UTC(), Command: line, Agent: agent, Effects: effs,
		Protection: r.Protection, Notes: r.Notes, Blocked: r.Blocked, Undo: r.Undo, Status: status,
	})
	if err != nil {
		// Without a journal entry, undo could not find the protection.
		return blocked("cannot write the journal: " + err.Error())
	}
	return outcome{
		Allowed: r.Allowed, ID: entry.ID, Effects: len(effs),
		Protection: r.Protection, Notes: r.Notes, Blocked: r.Blocked, CanUndo: len(r.Undo) > 0,
	}
}

// cmdProtect runs before the agent's command. Any failure, including its
// own, ends in exit 2 so the command does not run unprotected.
func cmdProtect(args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("protect", flag.ContinueOnError)
	fs.SetOutput(stderr)
	asJSON := fs.Bool("json", false, "print the result as JSON")
	agent := fs.String("agent", "", "which agent ran the command, for the log")
	if err := fs.Parse(args); err != nil {
		return exitBlock
	}
	line := strings.Join(fs.Args(), " ")
	if strings.TrimSpace(line) == "" {
		fmt.Fprintln(stderr, "cloudundo protect: no command given")
		return exitBlock
	}

	o := decide(line, *agent)
	if *asJSON {
		if o.Effects == 0 && o.Allowed {
			fmt.Fprintln(stdout, `{"allowed":true,"effects":[]}`)
		} else {
			out, _ := json.MarshalIndent(map[string]any{
				"id": o.ID, "allowed": o.Allowed, "protection": o.Protection,
				"notes": o.Notes, "blocked": o.Blocked,
			}, "", "  ")
			fmt.Fprintln(stdout, string(out))
		}
	}
	if !o.Allowed {
		if o.ID == 0 {
			fmt.Fprintln(stderr, "cloudundo: blocked:", strings.Join(o.Blocked, "; "))
		} else {
			fmt.Fprint(stderr, o.message())
		}
		return exitBlock
	}
	if !*asJSON && o.Effects > 0 {
		fmt.Fprint(stdout, o.message())
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
		fmt.Fprintf(stdout, "Started (%s). Run `cloudundo undo %d --yes` again in a few minutes to finish.\n", steps(n), id)
		return 0
	}
	fmt.Fprintf(stdout, "Restored (%s).\n", steps(n))
	return 0
}

func steps(n int) string {
	if n == 1 {
		return "1 step"
	}
	return fmt.Sprintf("%d steps", n)
}
