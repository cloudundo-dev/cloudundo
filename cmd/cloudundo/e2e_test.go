package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/cloudundo/cloudundo/internal/awscli"
	"github.com/cloudundo/cloudundo/internal/awstest"
	"github.com/cloudundo/cloudundo/internal/journal"
)

// TestEndToEnd runs cloudundo the way agents do: the built program, started
// by the plugin's hook script with the agent's JSON on stdin, calling an
// "aws" program (a stand-in that forwards to the fake AWS), several at
// once, then `cloudundo undo` from another process.
func TestEndToEnd(t *testing.T) {
	if testing.Short() {
		t.Skip("builds programs")
	}
	tmp := t.TempDir()
	bin := filepath.Join(tmp, "bin")
	build := func(out, pkg string) {
		t.Helper()
		cmd := exec.Command("go", "build", "-o", out, pkg)
		if b, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("build %s: %v\n%s", pkg, err, b)
		}
	}
	build(filepath.Join(bin, "cloudundo"), ".")
	build(filepath.Join(tmp, "aws"), "../../internal/awstest/awsproxy")

	f := awstest.New()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var req struct {
			Args []string `json:"args"`
			Env  []string `json:"env"`
		}
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			http.Error(w, err.Error(), 400)
			return
		}
		out, err := f.Run(context.Background(), req.Env, req.Args...)
		res := map[string]any{"stdout": out, "code": 0}
		if err != nil {
			var ae *awscli.Error
			if errors.As(err, &ae) {
				res["code"], res["stderr"] = ae.Code, ae.Stderr
			} else {
				res["code"], res["stderr"] = 255, err.Error()
			}
		}
		json.NewEncoder(w).Encode(res)
	}))
	defer srv.Close()

	home := filepath.Join(tmp, "home")
	env := []string{
		"PATH=" + bin + ":/usr/bin:/bin",
		"HOME=" + tmp,
		"CLOUDUNDO_HOME=" + home,
		"CLOUDUNDO_AWS_BIN=" + filepath.Join(tmp, "aws"),
		"CLOUDUNDO_FAKE_AWS_URL=" + srv.URL,
		"NO_PROXY=127.0.0.1,localhost", "no_proxy=127.0.0.1,localhost",
		"AWS_PROFILE=agent",
	}
	hook, err := filepath.Abs("../../plugin/scripts/hook.sh")
	if err != nil {
		t.Fatal(err)
	}
	run := func(stdin string, args ...string) (int, string, string) {
		cmd := exec.Command(args[0], args[1:]...)
		cmd.Env = env
		cmd.Dir = tmp
		cmd.Stdin = strings.NewReader(stdin)
		var out, errb strings.Builder
		cmd.Stdout, cmd.Stderr = &out, &errb
		err := cmd.Run()
		code := 0
		var ee *exec.ExitError
		if errors.As(err, &ee) {
			code = ee.ExitCode()
		} else if err != nil {
			t.Fatalf("%v: %v", args, err)
		}
		return code, out.String(), errb.String()
	}

	// What exists before the agent's commands, an hour ago.
	f.SetClock(time.Now().UTC().Add(-time.Hour))
	f.CreateBucket("uploads", "us-west-1", "")
	f.Put("uploads", "a.txt", "hello")
	f.Put("uploads", "b.txt", "world")
	for i := 0; i < 8; i++ {
		f.CreateBucket(fmt.Sprintf("b%d", i), "us-west-1", "")
		f.Put(fmt.Sprintf("b%d", i), "x", "x")
	}
	f.CreateTable("Users", "pk")
	f.PutItem("Users", `{"pk":{"S":"u1"},"name":{"S":"Ali"}}`)
	// The agent's commands happen from now on.
	f.SetClock(time.Now().UTC().Add(time.Minute))

	// 1. Claude Code, through the plugin's hook script.
	code, out, errOut := run(claudeInput("aws s3 rm s3://uploads --recursive"), "sh", hook, "claude-code")
	if code != 0 || !strings.Contains(out, "protected #1") {
		t.Fatalf("claude-code hook: exit %d\n%s%s", code, out, errOut)
	}
	f.DeletePrefix("uploads", "") // the command runs
	if _, ok := f.Get("uploads", "a.txt"); ok {
		t.Fatal("setup: delete did not happen")
	}
	code, out, errOut = run("", filepath.Join(bin, "cloudundo"), "undo", "1", "--yes")
	if code != 0 || !strings.Contains(out, "Restored") {
		t.Fatalf("undo 1: exit %d\n%s%s", code, out, errOut)
	}
	if v, ok := f.Get("uploads", "a.txt"); !ok || v != "hello" {
		t.Fatalf("a.txt after undo = %q, %v", v, ok)
	}

	// 2. Eight Codex commands at the same moment: eight entries, eight numbers.
	var wg sync.WaitGroup
	codes := make([]int, 8)
	outs := make([]string, 8)
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			c, o, e := run(claudeInput(fmt.Sprintf("aws s3 rm s3://b%d/x", i)), "sh", hook, "codex")
			codes[i], outs[i] = c, o+e
		}(i)
	}
	wg.Wait()
	for i, c := range codes {
		if c != 0 {
			t.Fatalf("parallel hook %d: exit %d\n%s", i, c, outs[i])
		}
	}

	// 3. Cursor: a command cloudundo cannot read is denied in Cursor's format.
	code, out, _ = run(cursorInput("aws s3 rm s3://$B --recursive"), "sh", hook, "cursor")
	if code != 2 || !strings.Contains(out, `"permission":"deny"`) {
		t.Fatalf("cursor hook: exit %d\n%s", code, out)
	}

	// 4. A DynamoDB item, deleted and put back.
	code, out, errOut = run(claudeInput(`aws dynamodb delete-item --table-name Users --key '{"pk":{"S":"u1"}}'`), "sh", hook, "claude-code")
	if code != 0 || !strings.Contains(out, "protected #") {
		t.Fatalf("delete-item hook: exit %d\n%s%s", code, out, errOut)
	}
	f.DeleteItem("Users", `{"pk":{"S":"u1"}}`)

	code, out, _ = run("", filepath.Join(bin, "cloudundo"), "log", "--json")
	var entries []journal.Entry
	if code != 0 || json.Unmarshal([]byte(out), &entries) != nil {
		t.Fatalf("log --json: exit %d\n%s", code, out)
	}
	if len(entries) != 11 { // uploads, 8 parallel, blocked, delete-item
		t.Fatalf("%d journal entries, want 11", len(entries))
	}
	ids := map[int]bool{}
	for _, e := range entries {
		if ids[e.ID] {
			t.Fatalf("two entries got #%d", e.ID)
		}
		ids[e.ID] = true
		for _, s := range e.Undo {
			if s.Conn.Env["AWS_PROFILE"] != "agent" {
				t.Fatalf("#%d did not keep the agent's profile: %+v", e.ID, s.Conn)
			}
		}
	}
	last := entries[len(entries)-1].ID
	code, out, errOut = run("", filepath.Join(bin, "cloudundo"), "undo", fmt.Sprint(last), "--yes")
	if code != 0 || !strings.Contains(out, "Restored") {
		t.Fatalf("undo %d: exit %d\n%s%s", last, code, out, errOut)
	}
	if !strings.Contains(f.Item("Users", `{"pk":{"S":"u1"}}`), "Ali") {
		t.Fatal("item not restored")
	}

	// 5. Your own agent: the Python example from the README, as written.
	py, err := exec.LookPath("python3")
	if err != nil {
		t.Log("no python3: README example not run")
		return
	}
	readme, err := os.ReadFile("../../README.md")
	if err != nil {
		t.Fatal(err)
	}
	_, after, ok := strings.Cut(string(readme), "### Your own agent")
	_, after, ok2 := strings.Cut(after, "```python\n")
	example, _, ok3 := strings.Cut(after, "```")
	if !ok || !ok2 || !ok3 {
		t.Fatal("README has no Python example under \"Your own agent\"")
	}
	f.SetClock(time.Now().UTC().Add(-time.Hour))
	f.Put("uploads", "c.txt", "keep me")
	f.SetClock(time.Now().UTC().Add(2 * time.Minute))
	script := example + `
import sys
print(run_agent_command(sys.argv[1], "."))
`
	ownEnv := append([]string{"PATH=" + bin + ":" + tmp + ":/usr/bin:/bin"}, env[1:]...)
	runPy := func(command string) string {
		cmd := exec.Command(py, "-I", "-c", script, command)
		cmd.Env, cmd.Dir = ownEnv, tmp
		b, err := cmd.CombinedOutput()
		if err != nil {
			t.Fatalf("python: %v\n%s", err, b)
		}
		return string(b)
	}
	out = runPy("aws s3api delete-object --bucket uploads --key c.txt")
	if !strings.Contains(out, "protected #") {
		t.Fatalf("own agent, delete: %s", out)
	}
	if _, ok := f.Get("uploads", "c.txt"); ok {
		t.Fatalf("own agent: the command did not run: %s", out)
	}
	out = runPy("aws s3 rm s3://$B --recursive")
	if !strings.HasPrefix(out, "Not run.") {
		t.Fatalf("own agent, unknown target: %s", out)
	}
	entries = nil
	_, out, _ = run("", filepath.Join(bin, "cloudundo"), "log", "--json")
	json.Unmarshal([]byte(out), &entries)
	id := 0
	for _, e := range entries {
		if strings.Contains(e.Command, "c.txt") {
			id = e.ID
		}
	}
	code, out, errOut = run("", filepath.Join(bin, "cloudundo"), "undo", fmt.Sprint(id), "--yes")
	if code != 0 || !strings.Contains(out, "Restored") {
		t.Fatalf("undo own agent #%d: exit %d\n%s%s", id, code, out, errOut)
	}
	if v, _ := f.Get("uploads", "c.txt"); v != "keep me" {
		t.Fatalf("c.txt after undo = %q", v)
	}
}
