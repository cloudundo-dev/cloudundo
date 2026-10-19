package effects

import (
	"os"
	"path/filepath"
	"strings"
)

// TerraformChange is a terraform (or OpenTofu) apply or destroy. What it
// deletes is only known from its plan, which protection asks Terraform
// for before the command runs.
const TerraformChange Kind = "terraform-change"

// Terraform describes a terraform command whose plan must be read.
type Terraform struct {
	// Bin is "terraform" or "tofu".
	Bin string `json:"bin"`
	// Dir is the working directory, after any cd and -chdir.
	Dir string `json:"dir"`
	// PlanArgs are the command's options that shape the plan
	// (-target, -var, -var-file, -replace, -exclude, -refresh).
	PlanArgs []string `json:"plan_args,omitempty"`
	// PlanFile is the saved plan an apply runs, if any, relative to Dir.
	PlanFile string `json:"plan_file,omitempty"`
	Destroy  bool   `json:"destroy,omitempty"`
}

var terraformBins = map[string]bool{"terraform": true, "tofu": true}

// planFlags are the terraform options that change what a plan contains.
var planFlags = map[string]bool{
	"target": true, "var": true, "var-file": true, "replace": true,
	"exclude": true, "refresh": true,
}

// terraformFlagsWithValue take the next word when not written as -x=y.
var terraformFlagsWithValue = map[string]bool{
	"target": true, "var": true, "var-file": true, "replace": true,
	"exclude": true, "lock-timeout": true, "parallelism": true, "state": true,
	"state-out": true, "backup": true,
}

// terraform reads "terraform [global options] apply|destroy [options] [plan]".
func terraform(args []string, dir string, dirKnown bool) []Effect {
	if len(args) == 0 || !terraformBins[filepath.Base(args[0])] {
		return nil
	}
	tf := Terraform{Bin: filepath.Base(args[0]), Dir: dir}
	i := 1
	for ; i < len(args) && strings.HasPrefix(args[i], "-"); i++ {
		if v, ok := strings.CutPrefix(args[i], "-chdir="); ok {
			tf.Dir = joinDir(tf.Dir, v)
			if looksDynamic(v) {
				dirKnown = false
			}
		}
	}
	if i >= len(args) {
		return nil
	}
	switch args[i] {
	case "destroy":
		tf.Destroy = true
	case "apply":
	default:
		return nil
	}
	e := Effect{Kind: TerraformChange, Service: "terraform", TF: &tf}
	rest := args[i+1:]
	for j := 0; j < len(rest); j++ {
		a := rest[j]
		if !strings.HasPrefix(a, "-") {
			tf.PlanFile = a
			continue
		}
		name := strings.TrimLeft(a, "-")
		value, hasValue := "", false
		if k, v, ok := strings.Cut(name, "="); ok {
			name, value, hasValue = k, v, true
		} else if terraformFlagsWithValue[name] && j+1 < len(rest) {
			value, hasValue = rest[j+1], true
			j++
		}
		switch {
		case name == "destroy":
			tf.Destroy = true
		case name == "state" || name == "state-out" || name == "backup":
			// Local state files elsewhere: the plan cannot be reproduced
			// faithfully, so do not guess.
			e.Uncertain = true
			e.Detail = "uses a custom -state file"
		case planFlags[name] && hasValue:
			tf.PlanArgs = append(tf.PlanArgs, "-"+name+"="+value)
			if looksDynamic(value) {
				e.Uncertain = true
			}
		}
	}
	if looksDynamic(tf.PlanFile) {
		e.Uncertain = true
	}
	if !dirKnown || looksDynamic(tf.Dir) {
		e.Uncertain = true
		if e.Detail == "" {
			e.Detail = "the working directory is not written in the command"
		}
	}
	return []Effect{e}
}

// joinDir resolves dir against base, expanding a leading ~.
func joinDir(base, dir string) string {
	if dir == "~" || strings.HasPrefix(dir, "~/") {
		if home, err := os.UserHomeDir(); err == nil {
			dir = filepath.Join(home, strings.TrimPrefix(dir, "~"))
		}
	}
	if filepath.IsAbs(dir) || base == "" {
		return filepath.Clean(dir)
	}
	return filepath.Join(base, dir)
}
