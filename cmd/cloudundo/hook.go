package main

import (
	"encoding/json"
	"fmt"
	"io"
	"os"
	"strings"
)

// stdin is swapped in tests.
var stdin io.Reader = os.Stdin

// maxHookInput bounds what a hook reads; agents send a few KB.
const maxHookInput = 16 << 20

// Agents cloudundo has hooks for.
const (
	agentClaude = "claude-code"
	agentCursor = "cursor"
	agentCodex  = "codex"
)

// blockHint goes to the agent with every block, so it stops instead of
// looking for another way to run the same change.
const blockHint = "Do not run this another way. Tell the user what was blocked and why."

// cmdHook is what an agent runs before each shell command. It reads the
// agent's JSON on stdin and answers in that agent's format.
//
// Every agent supported here treats exit code 2 as "block", while other
// failures may let the command through. So every problem cloudundo can
// see, including bad input and its own errors, ends in exit 2.
func cmdHook(args []string, stdout, stderr io.Writer) (code int) {
	defer func() {
		if r := recover(); r != nil {
			fmt.Fprintf(stderr, "cloudundo: blocked: internal error: %v\n%s\n", r, blockHint)
			code = exitBlock
		}
	}()
	if len(args) != 1 {
		fmt.Fprintln(stderr, "usage: cloudundo hook claude-code|cursor|codex")
		return exitBlock
	}
	agent := args[0]
	if agent != agentClaude && agent != agentCursor && agent != agentCodex {
		fmt.Fprintf(stderr, "cloudundo hook: unknown agent %q (want claude-code, cursor or codex)\n", agent)
		return exitBlock
	}
	block := func(msg string) int {
		msg = strings.TrimRight(msg, "\n") + "\n" + blockHint
		if agent == agentCursor {
			writeJSON(stdout, map[string]any{"permission": "deny", "user_message": msg, "agent_message": msg})
		}
		fmt.Fprintln(stderr, msg)
		return exitBlock
	}

	body, err := io.ReadAll(io.LimitReader(stdin, maxHookInput+1))
	if err != nil {
		return block("cloudundo: blocked: could not read the hook input: " + err.Error())
	}
	if len(body) > maxHookInput {
		return block("cloudundo: blocked: the hook input is too large to check")
	}
	line, isShell, err := hookCommand(agent, body)
	if err != nil {
		return block("cloudundo: blocked: " + err.Error())
	}
	if !isShell {
		return allow(agent, stdout, outcome{Allowed: true})
	}
	// Run where the agent runs the command, so relative paths (a
	// terraform directory, a plan file) mean the same thing.
	if dir := hookCwd(body); dir != "" {
		if st, err := os.Stat(dir); err == nil && st.IsDir() {
			if err := os.Chdir(dir); err != nil {
				return block("cloudundo: blocked: cannot enter " + dir + ": " + err.Error())
			}
		}
	}

	o := decide(line, agent)
	if !o.Allowed {
		return block(o.message())
	}
	return allow(agent, stdout, o)
}

// allow answers "no objection". It never approves on the user's behalf in
// Claude Code or Codex: the agent's own permission rules still apply.
func allow(agent string, stdout io.Writer, o outcome) int {
	msg := ""
	if o.Effects > 0 {
		msg = strings.TrimRight(o.message(), "\n")
	}
	switch agent {
	case agentCursor:
		out := map[string]any{"permission": "allow"}
		if msg != "" {
			out["agent_message"] = msg
			out["user_message"] = msg
		}
		writeJSON(stdout, out)
	case agentClaude:
		if msg != "" {
			writeJSON(stdout, map[string]any{
				"systemMessage": msg,
				"hookSpecificOutput": map[string]any{
					"hookEventName":     "PreToolUse",
					"additionalContext": msg,
				},
			})
		}
	}
	// Codex: no output. It rejects fields it does not support and has no
	// way to show a note, so the undo id is in `cloudundo log`.
	return 0
}

func writeJSON(w io.Writer, v any) {
	b, _ := json.Marshal(v)
	fmt.Fprintln(w, string(b))
}

// hookCommand pulls the shell command out of an agent's hook input.
// isShell is false for events about other tools, which cloudundo leaves
// alone.
func hookCommand(agent string, body []byte) (line string, isShell bool, err error) {
	var in map[string]json.RawMessage
	if err := json.Unmarshal(body, &in); err != nil || in == nil {
		return "", false, fmt.Errorf("the %s hook input is not a JSON object", agent)
	}

	if agent == agentCursor {
		if ev := jsonString(in["hook_event_name"]); ev != "" && ev != "beforeShellExecution" {
			return "", false, nil
		}
		cmd, ok := commandValue(in["command"])
		if !ok {
			return "", false, fmt.Errorf("the cursor hook input has no command")
		}
		return cmd, true, nil
	}

	// Claude Code and Codex share the PreToolUse shape.
	if _, ok := in["tool_name"]; !ok {
		return "", false, fmt.Errorf("the %s hook input has no tool_name", agent)
	}
	if jsonString(in["tool_name"]) != "Bash" {
		return "", false, nil
	}
	var input map[string]json.RawMessage
	if err := json.Unmarshal(in["tool_input"], &input); err != nil || input == nil {
		return "", false, fmt.Errorf("the %s hook input has no tool_input", agent)
	}
	cmd, ok := commandValue(input["command"])
	if !ok {
		return "", false, fmt.Errorf("the %s hook input has no command", agent)
	}
	return cmd, true, nil
}

func jsonString(raw json.RawMessage) string {
	var s string
	if json.Unmarshal(raw, &s) != nil {
		return ""
	}
	return s
}

// commandValue accepts a command as a string, or as an argv array, which
// is turned into one safely quoted shell line.
func commandValue(raw json.RawMessage) (string, bool) {
	if t := strings.TrimSpace(string(raw)); t == "" || t == "null" {
		return "", false
	}
	var s string
	if json.Unmarshal(raw, &s) == nil {
		return s, true
	}
	var argv []string
	if json.Unmarshal(raw, &argv) == nil && len(argv) > 0 {
		q := make([]string, len(argv))
		for i, a := range argv {
			q[i] = shellQuote(a)
		}
		return strings.Join(q, " "), true
	}
	return "", false
}

func shellQuote(s string) string {
	if s != "" && strings.IndexFunc(s, func(r rune) bool {
		return !(r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z' || r >= '0' && r <= '9' || strings.ContainsRune("-_./:=@,+%", r))
	}) < 0 {
		return s
	}
	return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'"
}

// hookCwd is the agent's working directory from the hook input, if given.
func hookCwd(body []byte) string {
	var in struct {
		Cwd string `json:"cwd"`
	}
	_ = json.Unmarshal(body, &in)
	return in.Cwd
}
