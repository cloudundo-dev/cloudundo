package main

import (
	"bytes"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
)

// Swapped in tests.
var (
	homeDir    = os.UserHomeDir
	executable = func() (string, error) {
		p, err := os.Executable()
		if err != nil {
			return "", err
		}
		if r, err := filepath.EvalSymlinks(p); err == nil {
			p = r
		}
		return p, nil
	}
)

// hookTimeout is long enough for a bucket copy or a table backup.
const hookTimeout = 600

// cmdInstall prints, or with --write adds, the hook settings for an agent.
// With no agent named, it sets up every agent it finds on this computer.
func cmdInstall(args []string, stdout, stderr io.Writer) int {
	fset := flag.NewFlagSet("install", flag.ContinueOnError)
	fset.SetOutput(stderr)
	write := fset.Bool("write", false, "add the hook to the agent's settings file (a backup is kept)")
	agent := ""
	if len(args) > 0 && !strings.HasPrefix(args[0], "-") {
		agent, args = args[0], args[1:]
	}
	if err := fset.Parse(args); err != nil {
		return 2
	}
	if agent == "" && fset.NArg() == 1 {
		agent = fset.Arg(0)
	} else if fset.NArg() > 0 {
		fmt.Fprintln(stderr, installUsage)
		return 2
	}

	home, err := homeDir()
	if err != nil {
		fmt.Fprintln(stderr, "cloudundo install:", err)
		return 1
	}
	exe, err := executable()
	if err != nil {
		fmt.Fprintln(stderr, "cloudundo install: cannot find the cloudundo program:", err)
		return 1
	}
	if agent != "" {
		return installOne(agent, *write, home, exe, stdout, stderr)
	}

	found := detectAgents(home)
	if len(found) == 0 {
		fmt.Fprintln(stderr, "cloudundo install: found no Claude Code, Cursor or Codex on this computer.")
		fmt.Fprintln(stderr, installUsage)
		return 2
	}
	if !*write {
		fmt.Fprintln(stdout, "Found:")
		for _, a := range found {
			fmt.Fprintf(stdout, "  %-12s hook goes in %s\n", agentNames[a], settingsFile(home, a))
		}
		fmt.Fprintln(stdout, "\nRun `cloudundo install --write` to add the hook to all of them (each old file is kept as *.before-cloudundo),")
		fmt.Fprintln(stdout, "or `cloudundo install <agent>` to see one agent's settings first.")
		return 0
	}
	code := 0
	for i, a := range found {
		if i > 0 {
			fmt.Fprintln(stdout)
		}
		fmt.Fprintf(stdout, "%s:\n", agentNames[a])
		if c := installOne(a, true, home, exe, stdout, stderr); c != 0 {
			code = c
		}
	}
	return code
}

const installUsage = "usage: cloudundo install [claude-code|cursor|codex] [--write]"

var agentNames = map[string]string{agentClaude: "Claude Code", agentCursor: "Cursor", agentCodex: "Codex"}

// lookPath finds a program; swapped in tests.
var lookPath = exec.LookPath

// detectAgents lists the agents that look installed: their settings
// folder exists (Cursor's server folder too, for Cursor connected to
// WSL or SSH) or their program is on PATH.
func detectAgents(home string) []string {
	has := func(paths ...string) bool {
		for _, p := range paths {
			if st, err := os.Stat(filepath.Join(home, p)); err == nil && st.IsDir() {
				return true
			}
		}
		return false
	}
	onPath := func(name string) bool {
		_, err := lookPath(name)
		return err == nil
	}
	var out []string
	if has(".claude") || onPath("claude") {
		out = append(out, agentClaude)
	}
	if has(".cursor", ".cursor-server") || onPath("cursor") {
		out = append(out, agentCursor)
	}
	if has(".codex") || onPath("codex") {
		out = append(out, agentCodex)
	}
	return out
}

func settingsFile(home, agent string) string {
	switch agent {
	case agentClaude:
		return filepath.Join(home, ".claude", "settings.json")
	case agentCursor:
		return filepath.Join(home, ".cursor", "hooks.json")
	}
	return filepath.Join(home, ".codex", "hooks.json")
}

// installOne prints, or with write adds, one agent's hook.
func installOne(agent string, write bool, home, exe string, stdout, stderr io.Writer) int {
	command := quoteExe(exe) + " hook " + agent

	var event string
	var entry map[string]any
	switch agent {
	case agentClaude:
		event = "PreToolUse"
		entry = map[string]any{"matcher": "Bash", "hooks": []any{map[string]any{
			"type": "command", "command": command, "timeout": hookTimeout, "onFailure": "block",
		}}}
	case agentCursor:
		event = "beforeShellExecution"
		entry = map[string]any{"command": command, "timeout": hookTimeout, "failClosed": true}
	case agentCodex:
		event = "PreToolUse"
		entry = map[string]any{"matcher": "^Bash$", "hooks": []any{map[string]any{
			"type": "command", "command": command, "timeout": hookTimeout,
		}}}
	default:
		fmt.Fprintln(stderr, installUsage)
		return 2
	}
	file := settingsFile(home, agent)

	if !write {
		snippet := map[string]any{"hooks": map[string]any{event: []any{entry}}}
		if agent == agentCursor {
			snippet["version"] = 1
		}
		b, _ := json.MarshalIndent(snippet, "", "  ")
		fmt.Fprintf(stdout, "Add this to %s:\n\n%s\n\nOr run `cloudundo install %s --write` to add it for you.\n", file, b, agent)
		printAfter(stdout, agent)
		return 0
	}

	backup, updated, err := addHook(file, event, entry, agent, command)
	switch {
	case errors.Is(err, errInstalled):
		fmt.Fprintf(stdout, "The cloudundo hook is already in %s.\n", file)
		return 0
	case err != nil:
		fmt.Fprintln(stderr, "cloudundo install:", err)
		return 1
	}
	if updated {
		fmt.Fprintf(stdout, "Updated the cloudundo hook in %s to run %s.\n", file, exe)
	} else {
		fmt.Fprintf(stdout, "Added the cloudundo hook to %s.\n", file)
	}
	if backup != "" {
		fmt.Fprintf(stdout, "The old file is kept at %s.\n", backup)
	}
	printAfter(stdout, agent)
	return 0
}

func printAfter(w io.Writer, agent string) {
	switch agent {
	case agentClaude:
		fmt.Fprintln(w, `"onFailure": "block" needs Claude Code v2.1.295 or later; it makes a hook that crashes or times out block the command.`)
	case agentCursor:
		fmt.Fprintln(w, "Quit and reopen Cursor so it loads the hook.")
	case agentCodex:
		fmt.Fprintln(w, "Run /hooks in Codex to review and trust the hook; Codex does not run it until you do.")
		fmt.Fprintln(w, "Codex lets a command through if the hook itself crashes or times out; cloudundo blocks on every error it can see.")
	}
}

var errInstalled = errors.New("already installed")

// addHook appends entry under hooks.<event> in a JSON settings file,
// keeping everything else. It refuses to touch a file it cannot parse.
// If a cloudundo hook is there already but runs another program (an older
// copy somewhere else), it is pointed at command instead, and updated is
// true.
func addHook(file, event string, entry map[string]any, agent, command string) (backup string, updated bool, err error) {
	settings := map[string]any{}
	old, err := os.ReadFile(file)
	mode := fs.FileMode(0o600)
	switch {
	case errors.Is(err, fs.ErrNotExist):
		old = nil
	case err != nil:
		return "", false, err
	default:
		if st, err := os.Stat(file); err == nil {
			mode = st.Mode().Perm()
		}
		if len(bytes.TrimSpace(old)) > 0 {
			if err := json.Unmarshal(old, &settings); err != nil || settings == nil {
				return "", false, fmt.Errorf("%s is not a JSON object, so it was not changed: %v", file, err)
			}
		}
	}

	hooks := map[string]any{}
	if h, ok := settings["hooks"]; ok {
		if hooks, ok = h.(map[string]any); !ok {
			return "", false, fmt.Errorf(`"hooks" in %s is not an object, so it was not changed`, file)
		}
	}
	var list []any
	if l, ok := hooks[event]; ok {
		if list, ok = l.([]any); !ok {
			return "", false, fmt.Errorf(`"hooks.%s" in %s is not a list, so it was not changed`, event, file)
		}
	}
	if found, changed := pointCloudundo(list, agent, command); found {
		if !changed {
			return "", false, errInstalled
		}
		updated = true
	} else {
		list = append(list, entry)
	}
	hooks[event] = list
	settings["hooks"] = hooks
	if agent == agentCursor {
		if _, ok := settings["version"]; !ok {
			settings["version"] = 1
		}
	}

	body, err := json.MarshalIndent(settings, "", "  ")
	if err != nil {
		return "", false, err
	}
	body = append(body, '\n')
	if err := os.MkdirAll(filepath.Dir(file), 0o755); err != nil {
		return "", false, err
	}
	if old != nil {
		backup = file + ".before-cloudundo"
		for i := 1; ; i++ {
			if _, err := os.Stat(backup); errors.Is(err, fs.ErrNotExist) {
				break
			}
			backup = fmt.Sprintf("%s.before-cloudundo.%d", file, i)
		}
		if err := os.WriteFile(backup, old, mode); err != nil {
			return "", false, fmt.Errorf("could not back up %s, so it was not changed: %w", file, err)
		}
	}
	tmp, err := os.CreateTemp(filepath.Dir(file), ".cloudundo-*")
	if err != nil {
		return "", false, err
	}
	defer os.Remove(tmp.Name())
	if _, err := tmp.Write(body); err != nil {
		tmp.Close()
		return "", false, err
	}
	if err := tmp.Chmod(mode); err != nil {
		tmp.Close()
		return "", false, err
	}
	if err := tmp.Close(); err != nil {
		return "", false, err
	}
	return backup, updated, os.Rename(tmp.Name(), file)
}

// pointCloudundo finds cloudundo's hook for agent in a hook list, at any
// depth, and makes each one run command. found is true when there was
// one; changed is true when one ran something else.
func pointCloudundo(v any, agent, command string) (found, changed bool) {
	switch x := v.(type) {
	case []any:
		for _, e := range x {
			f, c := pointCloudundo(e, agent, command)
			found, changed = found || f, changed || c
		}
	case map[string]any:
		if c, ok := x["command"].(string); ok && strings.Contains(c, "cloudundo") && strings.HasSuffix(strings.TrimSpace(c), "hook "+agent) {
			if c != command {
				x["command"] = command
				return true, true
			}
			return true, false
		}
		for _, e := range x {
			f, c := pointCloudundo(e, agent, command)
			found, changed = found || f, changed || c
		}
	}
	return found, changed
}

// quoteExe makes the program path safe in a shell command line.
func quoteExe(p string) string {
	if strings.ContainsAny(p, " \t'\"$`\\&;|<>()*?[]{}!#~") {
		if os.PathSeparator == '\\' {
			return `"` + p + `"`
		}
		return shellQuote(p)
	}
	return p
}
