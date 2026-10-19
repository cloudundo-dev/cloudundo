package main

import (
	"bytes"
	"strings"
	"testing"
)

func runCLI(t *testing.T, args ...string) (int, string, string) {
	t.Helper()
	var out, errb bytes.Buffer
	code := run(args, &out, &errb)
	return code, out.String(), errb.String()
}

func TestCheckRecordLogUndo(t *testing.T) {
	t.Setenv("CLOUDUNDO_HOME", t.TempDir())

	code, out, _ := runCLI(t, "check", "--record", "aws", "s3", "rb", "s3://user-data", "--force")
	if code != 0 {
		t.Fatalf("check exit %d", code)
	}
	if !strings.Contains(out, "Recorded as #1") || !strings.Contains(out, "DELETE BUCKET s3://user-data") {
		t.Fatalf("check output:\n%s", out)
	}

	code, out, _ = runCLI(t, "check", "--", "aws s3 ls")
	if code != 0 || !strings.Contains(out, "No data changes") {
		t.Fatalf("read-only check: %d\n%s", code, out)
	}

	code, out, _ = runCLI(t, "log")
	if code != 0 || !strings.Contains(out, "#1") || !strings.Contains(out, "with all objects") {
		t.Fatalf("log: %d\n%s", code, out)
	}

	// A check-only entry has nothing to undo.
	code, out, _ = runCLI(t, "undo", "1")
	if code != 0 || !strings.Contains(out, "Nothing to restore") {
		t.Fatalf("undo: %d\n%s", code, out)
	}

	code, _, _ = runCLI(t, "undo", "42")
	if code != 1 {
		t.Fatalf("undo of missing id: exit %d, want 1", code)
	}
}

func TestCheckJSON(t *testing.T) {
	code, out, _ := runCLI(t, "check", "--json", "aws s3 rm s3://b/k")
	if code != 0 || !strings.Contains(out, `"kind": "delete-objects"`) {
		t.Fatalf("json: %d\n%s", code, out)
	}
	code, out, _ = runCLI(t, "check", "--json", "echo hi")
	if code != 0 || strings.TrimSpace(out) != "[]" {
		t.Fatalf("empty json: %d %q", code, out)
	}
}

func TestUsage(t *testing.T) {
	if code, _, _ := runCLI(t); code != 2 {
		t.Fatalf("no args: exit %d", code)
	}
	if code, _, _ := runCLI(t, "nope"); code != 2 {
		t.Fatalf("unknown: exit %d", code)
	}
}
