package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"strings"
	"time"
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

	body, err := readHookInput(stdin)
	if err != nil {
		return block("cloudundo: blocked: " + err.Error())
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
	agentCwd = hookCwd(body)
	if dir := agentCwd; dir != "" {
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

// agentCwd is the folder the agent said it runs the command in, kept in
// the journal next to the folder cloudundo actually used.
var agentCwd string

// hookCwd is the agent's working directory from the hook input, if given.
func hookCwd(body []byte) string {
	var in struct {
		Cwd string `json:"cwd"`
	}
	_ = json.Unmarshal(body, &in)
	return in.Cwd
}

// hookReadTimeout bounds the wait for the agent's input, so a hook that
// gets none blocks quickly instead of holding the agent until its timeout.
var hookReadTimeout = 30 * time.Second

// countingReader counts what has been read through it.
type countingReader struct {
	r io.Reader
	n int64
}

func (c *countingReader) Read(p []byte) (int, error) {
	n, err := c.r.Read(p)
	c.n += int64(n)
	return n, err
}

// readHookInput reads the agent's JSON. It stops at the end of the first
// JSON value rather than waiting for the agent to close stdin, which some
// agents do late or not at all.
func readHookInput(r io.Reader) ([]byte, error) {
	type result struct {
		body []byte
		err  error
	}
	done := make(chan result, 1)
	go func() {
		// A panic here would not reach cmdHook's recover; turn it into an
		// error so the command is still blocked with a message.
		defer func() {
			if p := recover(); p != nil {
				done <- result{nil, fmt.Errorf("internal error reading the hook input: %v", p)}
			}
		}()
		cr := &countingReader{r: io.LimitReader(r, maxHookInput+1)}
		var raw json.RawMessage
		err := json.NewDecoder(cr).Decode(&raw)
		switch {
		case cr.n > maxHookInput:
			done <- result{nil, errors.New("the hook input is too large to check")}
		case err != nil && !errors.Is(err, io.EOF) && !errors.Is(err, io.ErrUnexpectedEOF):
			if _, isSyntax := err.(*json.SyntaxError); isSyntax {
				done <- result{[]byte("not json"), nil} // reported as "not a JSON object" below
				return
			}
			done <- result{nil, fmt.Errorf("could not read the hook input: %w", err)}
		case err != nil:
			done <- result{[]byte("not json"), nil}
		default:
			done <- result{raw, nil}
		}
	}()
	select {
	case res := <-done:
		return res.body, res.err
	case <-time.After(hookReadTimeout):
		return nil, fmt.Errorf("the agent sent no hook input within %s", hookReadTimeout)
	}
}
