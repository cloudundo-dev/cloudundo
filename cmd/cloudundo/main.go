// Command cloudundo makes what an AI agent deletes in AWS recoverable.
//
// "protect" runs before an agent's command: it makes the command's S3,
// DynamoDB and RDS changes recoverable, or blocks the command. "undo" puts
// the data back.
package main

import (
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"os"
	"strings"

	"github.com/cloudundo/cloudundo/internal/effects"
	"github.com/cloudundo/cloudundo/internal/journal"
)

var version = "0.0.1-dev"

func main() {
	os.Exit(run(os.Args[1:], os.Stdout, os.Stderr))
}

const usage = `cloudundo: make what your AI agent deletes in AWS recoverable.

Usage:
  cloudundo protect [--json] -- <command>   make the command's AWS data changes recoverable, or block it
                                           exit 0: safe to run   exit 2: do not run
  cloudundo undo <id> [--yes]               show what undo would restore; --yes restores it
  cloudundo log [--json]                    list recorded commands
  cloudundo check [--json] -- <command>     only show what a command would change
  cloudundo install <agent> [--write]       set up the hook for claude-code, cursor or codex
  cloudundo policy [--account ID] [--region R] [--keep-versions]
                                           print an IAM policy that stops the agent's keys from deleting backups
  cloudundo hook <agent>                    what the agent runs before each shell command (reads JSON on stdin)
  cloudundo version

Settings (environment):
  CLOUDUNDO_HOME            where the journal lives (default ~/.cloudundo)
  CLOUDUNDO_RETENTION_DAYS  undo window on buckets cloudundo versions (default 7)
  CLOUDUNDO_MAX_COPY_GB     largest bucket copied before a bucket delete (default 5)
  CLOUDUNDO_AWS_BIN         aws CLI to use (default "aws" on PATH)
`

func run(args []string, stdout, stderr io.Writer) int {
	if len(args) == 0 {
		fmt.Fprint(stderr, usage)
		return 2
	}
	switch args[0] {
	case "protect":
		return cmdProtect(args[1:], stdout, stderr)
	case "check":
		return cmdCheck(args[1:], stdout, stderr)
	case "log":
		return cmdLog(args[1:], stdout, stderr)
	case "undo":
		return cmdUndo(args[1:], stdout, stderr)
	case "hook":
		return cmdHook(args[1:], stdout, stderr)
	case "install":
		return cmdInstall(args[1:], stdout, stderr)
	case "policy":
		return cmdPolicy(args[1:], stdout, stderr)
	case "version", "--version":
		fmt.Fprintln(stdout, "cloudundo", version)
		return 0
	case "help", "-h", "--help":
		fmt.Fprint(stdout, usage)
		return 0
	default:
		fmt.Fprintf(stderr, "cloudundo: unknown command %q\n\n%s", args[0], usage)
		return 2
	}
}

func cmdCheck(args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("check", flag.ContinueOnError)
	fs.SetOutput(stderr)
	asJSON := fs.Bool("json", false, "print effects as JSON")
	record := fs.Bool("record", false, "add the command to the journal")
	if err := fs.Parse(args); err != nil {
		return 2
	}
	line := strings.Join(fs.Args(), " ")
	if strings.TrimSpace(line) == "" {
		fmt.Fprintln(stderr, "cloudundo check: no command given")
		return 2
	}

	effs := effects.Analyze(line)

	if *record && len(effs) > 0 {
		j, err := openJournal()
		if err != nil {
			fmt.Fprintln(stderr, "cloudundo:", err)
			return 1
		}
		e, err := j.Append(journal.Entry{Command: line, Effects: effs, Status: "seen"})
		if err != nil {
			fmt.Fprintln(stderr, "cloudundo:", err)
			return 1
		}
		if !*asJSON {
			fmt.Fprintf(stdout, "Recorded as #%d\n", e.ID)
		}
	}

	if *asJSON {
		if effs == nil {
			effs = []effects.Effect{}
		}
		enc := json.NewEncoder(stdout)
		enc.SetIndent("", "  ")
		if err := enc.Encode(effs); err != nil {
			fmt.Fprintln(stderr, "cloudundo:", err)
			return 1
		}
		return 0
	}

	if len(effs) == 0 {
		fmt.Fprintln(stdout, "No data changes found.")
		return 0
	}
	for _, e := range effs {
		fmt.Fprintln(stdout, describe(e))
	}
	return 0
}

func cmdLog(args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("log", flag.ContinueOnError)
	fs.SetOutput(stderr)
	asJSON := fs.Bool("json", false, "print entries as JSON")
	if err := fs.Parse(args); err != nil {
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
	if *asJSON {
		if entries == nil {
			entries = []journal.Entry{}
		}
		enc := json.NewEncoder(stdout)
		enc.SetIndent("", "  ")
		_ = enc.Encode(entries)
		return 0
	}
	if len(entries) == 0 {
		fmt.Fprintln(stdout, "Nothing recorded yet.")
		return 0
	}
	// Newest first.
	for i := len(entries) - 1; i >= 0; i-- {
		e := entries[i]
		fmt.Fprintf(stdout, "#%-4d %s  %-6s %s\n", e.ID, e.Time.Local().Format("2006-01-02 15:04"), e.Status, e.Command)
		for _, eff := range e.Effects {
			fmt.Fprintln(stdout, "        "+describe(eff))
		}
	}
	return 0
}

func openJournal() (*journal.Journal, error) {
	dir, err := journal.DefaultDir()
	if err != nil {
		return nil, err
	}
	return journal.Open(dir)
}

func describe(e effects.Effect) string {
	var verb string
	switch e.Kind {
	case effects.DeleteObjects:
		verb = "DELETE    "
	case effects.DeleteBucket:
		verb = "DELETE BUCKET"
	case effects.OverwriteObjects:
		verb = "OVERWRITE "
	case effects.WeakenProtection:
		verb = "WEAKEN    "
	case effects.DeleteTable:
		verb = "DELETE TABLE"
	case effects.DeleteItems:
		verb = "DELETE    "
	case effects.OverwriteItems:
		verb = "OVERWRITE "
	case effects.DeleteDatabase:
		verb = "DELETE DATABASE"
	case effects.DeleteCache:
		verb = "DELETE CACHE"
	case effects.EraseCacheData:
		verb = "ERASE CACHE"
	case effects.TerraformChange:
		verb = "TERRAFORM "
	case effects.StackChange:
		verb = "CLOUDFORMATION"
	case effects.UnseenDelete:
		verb = "DELETE (unseen)"
	default:
		verb = string(e.Kind)
	}

	var target string
	switch {
	case e.Service == "rds":
		target = "rds:" + e.Database
		if e.Cluster {
			target = "rds-cluster:" + e.Database
		}
		if e.SkipFinalSnapshot {
			target += " (no final snapshot)"
		} else if e.FinalSnapshot != "" {
			target += " (final snapshot " + e.FinalSnapshot + ")"
		}
	case e.Service == "elasticache":
		target = "elasticache:" + e.Cache
		if e.FinalSnapshot != "" {
			target += " (final snapshot " + e.FinalSnapshot + ")"
		}
	case e.Service == "redis":
		target = "redis server"
	case e.Service == "terraform" && e.TF != nil:
		target = "apply (deletions read from its plan before it runs)"
		if e.TF.Destroy {
			target = "destroy (what it deletes is read from its plan before it runs)"
		}
		if e.TF.Dir != "" {
			target += " in " + e.TF.Dir
		}
	case e.Kind == effects.UnseenDelete:
		target = e.Command
	case e.Stack != nil:
		target = "stack " + e.Stack.Name
		switch e.Stack.Op {
		case "delete-stack":
			target = "delete-stack " + e.Stack.Name + " (its resources are read before it runs)"
		case "execute-change-set":
			target = "execute-change-set " + e.Stack.ChangeSet + " (its changes are read before it runs)"
		case "deploy":
			target = "deploy " + e.Stack.Name + " (its change set is read before it runs)"
		}
	case e.Service == "dynamodb":
		target = "dynamodb:" + e.Table
		if e.Table == "" || e.Table == "?" {
			target = "dynamodb:<unknown table>"
		}
		if n := len(e.Items); n > 0 {
			target += fmt.Sprintf(" (%d item(s))", n)
		}
	case e.Bucket == "":
		target = "s3://<unknown bucket>"
	default:
		target = "s3://" + e.Bucket
	}
	switch {
	case e.Service != "s3":
	case e.Key != "":
		target += "/" + e.Key
	case e.Recursive:
		target += "/" + e.Prefix + "*"
	case len(e.Keys) > 0:
		target += fmt.Sprintf(" (%d keys)", len(e.Keys))
	}

	var notes []string
	if e.Force {
		notes = append(notes, "with all objects")
	}
	if e.Filtered {
		notes = append(notes, "filtered by --include/--exclude")
	}
	if e.Uncertain {
		notes = append(notes, "target not fully known")
	}
	if e.Detail != "" {
		notes = append(notes, e.Detail)
	}
	s := verb + " " + target
	if len(notes) > 0 {
		s += "  (" + strings.Join(notes, "; ") + ")"
	}
	return s
}
