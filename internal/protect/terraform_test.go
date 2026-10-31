package protect

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/cloudundo-dev/cloudundo/internal/awscli"
	"github.com/cloudundo-dev/cloudundo/internal/effects"
)

// fakeTerraform writes a stand-in terraform that logs how it was called.
func fakeTerraform(t *testing.T, script string) (bin, log string) {
	t.Helper()
	d := t.TempDir()
	log = filepath.Join(d, "log")
	bin = filepath.Join(d, "terraform")
	body := "#!/bin/sh\necho \"$PWD|$AWS_PROFILE|$TF_INPUT|$*\" >> " + log + "\n" + script
	if err := os.WriteFile(bin, []byte(body), 0o755); err != nil {
		t.Fatal(err)
	}
	return bin, log
}

func TestExecPlannerMakesAPlanItNeverApplies(t *testing.T) {
	bin, log := fakeTerraform(t, `case "$1" in show) echo '{"resource_changes":[]}';; esac`+"\n")
	dir := t.TempDir()
	conn := awscli.ConnFromEnv(map[string]string{"AWS_PROFILE": "dev"})
	out, err := ExecPlanner(context.Background(), effects.Terraform{Bin: bin, Dir: dir, Destroy: true, PlanArgs: []string{"-target=aws_s3_bucket.b"}}, conn)
	if err != nil || !strings.Contains(string(out), "resource_changes") {
		t.Fatalf("out %q err %v", out, err)
	}
	b, _ := os.ReadFile(log)
	lines := strings.Split(strings.TrimSpace(string(b)), "\n")
	if len(lines) != 2 {
		t.Fatalf("calls:\n%s", b)
	}
	plan := lines[0]
	for _, want := range []string{dir + "|dev|0|plan ", "-input=false", "-lock=false", "-destroy", "-target=aws_s3_bucket.b", "-out="} {
		if !strings.Contains(plan, want) {
			t.Errorf("plan call %q lacks %q", plan, want)
		}
	}
	if !strings.Contains(lines[1], "|show -json -no-color ") {
		t.Errorf("show call %q", lines[1])
	}
	for _, l := range lines {
		sub := strings.Fields(l[strings.LastIndex(l, "|")+1:])[0]
		if sub != "plan" && sub != "show" {
			t.Fatalf("cloudundo must only plan and show, never %q: %q", sub, l)
		}
	}
}

func TestExecPlannerReadsASavedPlan(t *testing.T) {
	bin, log := fakeTerraform(t, `echo '{"resource_changes":[]}'`+"\n")
	dir := t.TempDir()
	if _, err := ExecPlanner(context.Background(), effects.Terraform{Bin: bin, Dir: dir, PlanFile: "tfplan"}, awscli.Conn{}); err != nil {
		t.Fatal(err)
	}
	b, _ := os.ReadFile(log)
	if got := strings.TrimSpace(string(b)); !strings.HasSuffix(got, "|show -json -no-color tfplan") || strings.Count(got, "\n") != 0 {
		t.Fatalf("calls: %q", got)
	}
}

func TestExecPlannerReportsFailure(t *testing.T) {
	bin, _ := fakeTerraform(t, "echo 'Error: Backend initialization required' >&2; exit 1\n")
	_, err := ExecPlanner(context.Background(), effects.Terraform{Bin: bin, Dir: t.TempDir(), Destroy: true}, awscli.Conn{})
	if err == nil || !strings.Contains(err.Error(), "Backend initialization required") {
		t.Fatalf("err = %v", err)
	}
	if _, err := ExecPlanner(context.Background(), effects.Terraform{Bin: filepath.Join(t.TempDir(), "none"), Dir: t.TempDir()}, awscli.Conn{}); err == nil {
		t.Fatal("missing terraform must fail")
	}
}
