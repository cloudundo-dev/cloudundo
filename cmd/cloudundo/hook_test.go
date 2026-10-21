package main

import (
	"encoding/json"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/cloudundo/cloudundo/internal/journal"
)

func hookRun(t *testing.T, agent, input string) (int, string, string) {
	t.Helper()
	old := stdin
	stdin = strings.NewReader(input)
	t.Cleanup(func() { stdin = old })
	return runCLI(t, "hook", agent)
}

func claudeInput(cmd string) string {
	b, _ := json.Marshal(map[string]any{
		"session_id": "s", "hook_event_name": "PreToolUse", "tool_name": "Bash",
		"tool_input": map[string]any{"command": cmd, "description": "x"},
	})
	return string(b)
}

func cursorInput(cmd string) string {
	b, _ := json.Marshal(map[string]any{
		"conversation_id": "c", "hook_event_name": "beforeShellExecution", "command": cmd, "cwd": "/w",
	})
	return string(b)
}

func cursorAnswer(t *testing.T, out string) map[string]any {
	t.Helper()
	var m map[string]any
	if err := json.Unmarshal([]byte(out), &m); err != nil {
		t.Fatalf("cursor output is not JSON: %q", out)
	}
	return m
}

func TestHookProtectsAndAllows(t *testing.T) {
	for _, agent := range []string{agentClaude, agentCodex, agentCursor} {
		t.Run(agent, func(t *testing.T) {
			f := withFake(t)
			f.CreateBucket("uploads", "us-east-1", "")
			f.Put("uploads", "a", "A")
			in := claudeInput("aws s3 rm s3://uploads/a")
			if agent == agentCursor {
				in = cursorInput("aws s3 rm s3://uploads/a")
			}
			code, out, errOut := hookRun(t, agent, in)
			if code != 0 {
				t.Fatalf("exit %d\n%s%s", code, out, errOut)
			}
			if f.Versioning("uploads") != "Enabled" {
				t.Fatal("bucket not protected")
			}
			switch agent {
			case agentCursor:
				m := cursorAnswer(t, out)
				if m["permission"] != "allow" || !strings.Contains(m["agent_message"].(string), "cloudundo undo 1") {
					t.Fatalf("cursor answer = %v", m)
				}
			case agentClaude:
				var m struct {
					SystemMessage      string
					HookSpecificOutput map[string]any
				}
				if err := json.Unmarshal([]byte(out), &m); err != nil || !strings.Contains(m.SystemMessage, "cloudundo undo 1") {
					t.Fatalf("claude answer = %q", out)
				}
				if _, ok := m.HookSpecificOutput["permissionDecision"]; ok {
					t.Fatal("the hook must not approve commands on the user's behalf")
				}
			case agentCodex:
				if out != "" {
					t.Fatalf("codex output = %q", out)
				}
			}
			entries := journalEntries(t)
			if len(entries) != 1 || entries[0].Agent != agent || entries[0].Status != "protected" {
				t.Fatalf("journal = %+v", entries)
			}
		})
	}
}

func TestHookBlocks(t *testing.T) {
	for _, agent := range []string{agentClaude, agentCodex, agentCursor} {
		t.Run(agent, func(t *testing.T) {
			f := withFake(t)
			in := claudeInput("aws s3 rm s3://$B --recursive")
			if agent == agentCursor {
				in = cursorInput("aws s3 rm s3://$B --recursive")
			}
			code, out, errOut := hookRun(t, agent, in)
			if code != exitBlock || !strings.Contains(errOut, "blocked #1") || !strings.Contains(errOut, blockHint) {
				t.Fatalf("exit %d\n%s%s", code, out, errOut)
			}
			if agent == agentCursor {
				if m := cursorAnswer(t, out); m["permission"] != "deny" || !strings.Contains(m["agent_message"].(string), "blocked") {
					t.Fatalf("cursor answer = %v", m)
				}
			}
			if len(f.Calls) != 0 {
				t.Fatalf("AWS called: %v", f.Calls)
			}
		})
	}
}

func TestHookLeavesOtherCommandsAlone(t *testing.T) {
	for _, agent := range []string{agentClaude, agentCodex, agentCursor} {
		f := withFake(t)
		in := claudeInput("npm test && git status")
		if agent == agentCursor {
			in = cursorInput("npm test && git status")
		}
		code, out, _ := hookRun(t, agent, in)
		if code != 0 || len(f.Calls) != 0 {
			t.Fatalf("%s: exit %d", agent, code)
		}
		if agent == agentCursor {
			if m := cursorAnswer(t, out); m["permission"] != "allow" {
				t.Fatalf("cursor answer = %v", m)
			}
		} else if out != "" {
			t.Fatalf("%s output = %q", agent, out)
		}
		if len(journalEntries(t)) != 0 {
			t.Fatalf("%s: harmless command was recorded", agent)
		}
	}
}

func TestHookIgnoresOtherTools(t *testing.T) {
	withFake(t)
	for _, agent := range []string{agentClaude, agentCodex} {
		in := `{"hook_event_name":"PreToolUse","tool_name":"Edit","tool_input":{"file_path":"x"}}`
		if code, _, errOut := hookRun(t, agent, in); code != 0 {
			t.Fatalf("%s: exit %d %s", agent, code, errOut)
		}
	}
	if code, out, _ := hookRun(t, agentCursor, `{"hook_event_name":"beforeReadFile","file_path":"x"}`); code != 0 || cursorAnswer(t, out)["permission"] != "allow" {
		t.Fatalf("cursor other event: %d %s", code, out)
	}
}

func TestHookBlocksBadInput(t *testing.T) {
	withFake(t)
	bad := []string{
		"",
		"not json",
		"null",
		"[]",
		`{"tool_name":"Bash"}`,
		`{"tool_name":"Bash","tool_input":null}`,
		`{"tool_name":"Bash","tool_input":{}}`,
		`{"tool_name":"Bash","tool_input":{"command":5}}`,
		`{"tool_name":"Bash","tool_input":{"command":null}}`,
		`{"tool_name":"Bash","tool_input":{"command":[]}}`,
		`{"tool_input":{"command":"aws s3 rb s3://b --force"}}`,
		`{"tool_name":"Bash","tool_input":{"command":"aws s3 rb s3://b --force"}`, // cut off
	}
	for _, agent := range []string{agentClaude, agentCodex} {
		for _, in := range bad {
			if code, _, _ := hookRun(t, agent, in); code != exitBlock {
				t.Errorf("%s %q: exit %d, want %d", agent, in, code, exitBlock)
			}
		}
	}
	for _, in := range []string{"", "nope", "null", `{"hook_event_name":"beforeShellExecution"}`, `{"command":null}`, `{"command":{"x":1}}`} {
		code, out, _ := hookRun(t, agentCursor, in)
		if code != exitBlock || cursorAnswer(t, out)["permission"] != "deny" {
			t.Errorf("cursor %q: exit %d %s", in, code, out)
		}
	}
}

func TestHookBlocksUnknownAgent(t *testing.T) {
	withFake(t)
	if code, _, _ := hookRun(t, "copilot", claudeInput("ls")); code != exitBlock {
		t.Fatalf("exit %d", code)
	}
	if code, _, _ := runCLI(t, "hook"); code != exitBlock {
		t.Fatalf("no agent: exit %d", code)
	}
}

func TestHookBlocksWhenInputCannotBeRead(t *testing.T) {
	withFake(t)
	old := stdin
	stdin = errReader{}
	t.Cleanup(func() { stdin = old })
	if code, _, errOut := runCLI(t, "hook", agentClaude); code != exitBlock || !strings.Contains(errOut, "could not read") {
		t.Fatalf("exit %d %s", code, errOut)
	}
}

type errReader struct{}

func (errReader) Read([]byte) (int, error) { return 0, errors.New("broken pipe") }

func TestHookBlocksOnTooMuchInput(t *testing.T) {
	withFake(t)
	old := stdin
	stdin = io.MultiReader(strings.NewReader(`{"tool_name":"Bash","tool_input":{"command":"`), strings.NewReader(strings.Repeat("a", maxHookInput)))
	t.Cleanup(func() { stdin = old })
	if code, _, errOut := runCLI(t, "hook", agentClaude); code != exitBlock || !strings.Contains(errOut, "too large") {
		t.Fatalf("exit %d %s", code, errOut)
	}
}

func TestHookBlocksWhenJournalFails(t *testing.T) {
	f := withFake(t)
	f.CreateBucket("b", "us-east-1", "")
	t.Setenv("CLOUDUNDO_HOME", "/proc/cloudundo-cannot-write")
	if code, _, errOut := hookRun(t, agentClaude, claudeInput("aws s3 rm s3://b/k")); code != exitBlock || !strings.Contains(errOut, "journal") {
		t.Fatalf("exit %d %s", code, errOut)
	}
}

func TestHookBlocksOnPanic(t *testing.T) {
	withFake(t)
	old := newRunner
	newRunner = nil // decide will panic calling it
	t.Cleanup(func() { newRunner = old })
	code, _, errOut := hookRun(t, agentCodex, claudeInput("aws s3 rm s3://b/k"))
	if code != exitBlock || !strings.Contains(errOut, "internal error") {
		t.Fatalf("exit %d %s", code, errOut)
	}
}

func TestHookAcceptsArgvCommands(t *testing.T) {
	f := withFake(t)
	in := `{"tool_name":"Bash","tool_input":{"command":["bash","-lc","aws s3 rb s3://$X --force"]}}`
	if code, _, errOut := hookRun(t, agentCodex, in); code != exitBlock || !strings.Contains(errOut, "blocked") {
		t.Fatalf("exit %d %s", code, errOut)
	}
	if len(f.Calls) != 0 {
		t.Fatal("AWS called")
	}
}

func journalEntries(t *testing.T) []journal.Entry {
	t.Helper()
	dir, err := journal.DefaultDir()
	if err != nil {
		t.Fatal(err)
	}
	j, err := journal.Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	es, err := j.List()
	if err != nil {
		t.Fatal(err)
	}
	return es
}

func TestCommandValueQuoting(t *testing.T) {
	got, ok := commandValue(json.RawMessage(`["aws","s3","rm","s3://b/it's here"]`))
	if !ok || got != `aws s3 rm 's3://b/it'\''s here'` {
		t.Fatalf("got %q", got)
	}
}

func withInstallHome(t *testing.T) string {
	t.Helper()
	home := t.TempDir()
	oldHome, oldExe := homeDir, executable
	homeDir = func() (string, error) { return home, nil }
	executable = func() (string, error) { return "/opt/my tools/cloudundo", nil }
	t.Cleanup(func() { homeDir, executable = oldHome, oldExe })
	return home
}

func TestInstallPrintsWithoutWriting(t *testing.T) {
	home := withInstallHome(t)
	code, out, _ := runCLI(t, "install", "claude-code")
	if code != 0 || !strings.Contains(out, `'/opt/my tools/cloudundo' hook claude-code`) || !strings.Contains(out, `"onFailure": "block"`) {
		t.Fatalf("exit %d\n%s", code, out)
	}
	if _, err := os.Stat(filepath.Join(home, ".claude")); err == nil {
		t.Fatal("install without --write changed files")
	}
}

func TestInstallMergesAndKeepsABackup(t *testing.T) {
	home := withInstallHome(t)
	file := filepath.Join(home, ".claude", "settings.json")
	os.MkdirAll(filepath.Dir(file), 0o755)
	orig := `{"model":"opus","hooks":{"PreToolUse":[{"matcher":"Edit","hooks":[{"type":"command","command":"lint"}]}],"Stop":[]}}`
	os.WriteFile(file, []byte(orig), 0o640)

	code, out, errOut := runCLI(t, "install", "claude-code", "--write")
	if code != 0 {
		t.Fatalf("exit %d\n%s%s", code, out, errOut)
	}
	var s map[string]any
	b, _ := os.ReadFile(file)
	if err := json.Unmarshal(b, &s); err != nil {
		t.Fatal(err)
	}
	pre := s["hooks"].(map[string]any)["PreToolUse"].([]any)
	if s["model"] != "opus" || len(pre) != 2 || s["hooks"].(map[string]any)["Stop"] == nil {
		t.Fatalf("settings = %s", b)
	}
	if !strings.Contains(string(b), "hook claude-code") {
		t.Fatalf("hook missing: %s", b)
	}
	if bk, _ := os.ReadFile(file + ".before-cloudundo"); string(bk) != orig {
		t.Fatalf("backup = %q", bk)
	}
	if st, _ := os.Stat(file); st.Mode().Perm() != 0o640 {
		t.Fatalf("mode changed to %v", st.Mode())
	}

	// Again: nothing added twice.
	code, out, _ = runCLI(t, "install", "claude-code", "--write")
	if code != 0 || !strings.Contains(out, "already") {
		t.Fatalf("second install: %d %s", code, out)
	}
	b2, _ := os.ReadFile(file)
	if string(b2) != string(b) {
		t.Fatal("second install changed the file")
	}
}

func TestInstallCursorAndCodexFromScratch(t *testing.T) {
	home := withInstallHome(t)
	for agent, rel := range map[string]string{agentCursor: ".cursor/hooks.json", agentCodex: ".codex/hooks.json"} {
		if code, out, errOut := runCLI(t, "install", agent, "--write"); code != 0 {
			t.Fatalf("%s: %d %s%s", agent, code, out, errOut)
		}
		b, err := os.ReadFile(filepath.Join(home, rel))
		if err != nil {
			t.Fatal(err)
		}
		var s map[string]any
		if err := json.Unmarshal(b, &s); err != nil {
			t.Fatal(err)
		}
		if agent == agentCursor {
			e := s["hooks"].(map[string]any)["beforeShellExecution"].([]any)[0].(map[string]any)
			if s["version"] != float64(1) || e["failClosed"] != true || !strings.HasSuffix(e["command"].(string), "hook cursor") {
				t.Fatalf("cursor = %s", b)
			}
		} else {
			g := s["hooks"].(map[string]any)["PreToolUse"].([]any)[0].(map[string]any)
			if g["matcher"] != "^Bash$" || !strings.Contains(string(b), "hook codex") {
				t.Fatalf("codex = %s", b)
			}
		}
		if _, err := os.Stat(filepath.Join(home, rel+".before-cloudundo")); err == nil {
			t.Fatalf("%s: backup of a file that did not exist", agent)
		}
	}
}

func TestInstallRefusesBrokenSettings(t *testing.T) {
	home := withInstallHome(t)
	for _, body := range []string{`{"hooks": `, `[1,2]`, `{"hooks":[]}`, `{"hooks":{"PreToolUse":{}}}`} {
		file := filepath.Join(home, ".claude", "settings.json")
		os.MkdirAll(filepath.Dir(file), 0o755)
		os.WriteFile(file, []byte(body), 0o600)
		code, _, errOut := runCLI(t, "install", "claude-code", "--write")
		if code == 0 || !strings.Contains(errOut, "not changed") {
			t.Fatalf("%q: exit %d %s", body, code, errOut)
		}
		if b, _ := os.ReadFile(file); string(b) != body {
			t.Fatalf("%q was changed to %q", body, b)
		}
	}
}

func TestInstallUnknownAgent(t *testing.T) {
	withInstallHome(t)
	if code, _, _ := runCLI(t, "install", "copilot"); code != 2 {
		t.Fatalf("exit %d", code)
	}
}

func FuzzHookInput(f *testing.F) {
	for _, s := range []string{claudeInput("aws s3 rm s3://b/k"), cursorInput("ls"), `{"tool_name":"Bash","tool_input":{"command":["a","b"]}}`, "null", "{", `{"command":null}`} {
		f.Add(s)
	}
	f.Fuzz(func(t *testing.T, in string) {
		for _, agent := range []string{agentClaude, agentCursor, agentCodex} {
			line, isShell, err := hookCommand(agent, []byte(in))
			if err != nil && (isShell || line != "") {
				t.Fatalf("%s %q: error with a command", agent, in)
			}
		}
	})
}

func TestHookBlocksOnPanicOutsideProtection(t *testing.T) {
	withFake(t)
	old := stdin
	stdin = nil // reading panics
	t.Cleanup(func() { stdin = old })
	if code, _, errOut := runCLI(t, "hook", agentCursor); code != exitBlock || !strings.Contains(errOut, "internal error") {
		t.Fatalf("exit %d %s", code, errOut)
	}
}

func TestHookRunsInTheAgentsDirectory(t *testing.T) {
	withFake(t)
	bin := t.TempDir()
	log := filepath.Join(bin, "log")
	script := "#!/bin/sh\necho \"$PWD\" >> " + log + "\ncase \"$1\" in show) echo '{\"resource_changes\":[]}';; esac\n"
	if err := os.WriteFile(filepath.Join(bin, "terraform"), []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", bin+string(os.PathListSeparator)+os.Getenv("PATH"))
	project, _ := filepath.EvalSymlinks(t.TempDir())
	t.Chdir(t.TempDir())
	in, _ := json.Marshal(map[string]any{"tool_name": "Bash", "cwd": project, "tool_input": map[string]any{"command": "terraform destroy -auto-approve"}})
	if code, _, errOut := hookRun(t, agentClaude, string(in)); code != 0 {
		t.Fatalf("exit %d %s", code, errOut)
	}
	b, _ := os.ReadFile(log)
	if got := strings.TrimSpace(strings.Split(string(b), "\n")[0]); got != project {
		t.Fatalf("terraform ran in %q, want %q", got, project)
	}
}

// Cursor also runs the Claude Code hooks in ~/.claude/settings.json, but
// sends them its own tool name ("Shell") and an empty cwd. In Cursor the
// protection comes from cloudundo's Cursor hook, which gets the real
// directory, so the Claude Code hook leaves these calls alone instead of
// protecting the command a second time in the wrong directory.
func TestClaudeHookLeavesCursorShellCallsToTheCursorHook(t *testing.T) {
	f := withFake(t)
	f.CreateBucket("uploads", "us-east-1", "")
	f.Put("uploads", "a", "A")
	b, _ := json.Marshal(map[string]any{
		"hook_event_name": "PreToolUse", "tool_name": "Shell", "cwd": "",
		"tool_input": map[string]any{"command": "aws s3 rm s3://uploads/a", "cwd": ""},
	})
	code, out, errOut := hookRun(t, agentClaude, string(b))
	if code != 0 || out != "" || errOut != "" {
		t.Fatalf("exit %d, out %q, err %q; want a silent pass", code, out, errOut)
	}
	if f.Versioning("uploads") != "" {
		t.Fatal("the Claude Code hook changed the bucket; the Cursor hook should do that")
	}
}
