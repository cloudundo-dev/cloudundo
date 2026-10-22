package effects

import (
	"path/filepath"
	"regexp"
	"strings"
)

// StackChange is a CloudFormation command that may delete resources: a
// stack delete, a change set run, or a deploy. What it deletes is only
// known from the stack (or the change set), which protection reads from
// AWS before the command runs.
const StackChange Kind = "stack-change"

// UnseenDelete is a command that deletes cloud resources through a tool
// whose plan cloudundo cannot read yet (cdk destroy, pulumi destroy...).
// It is blocked; Detail says what to use instead.
const UnseenDelete Kind = "unseen-delete"

// Stack describes a CloudFormation command (Kind StackChange).
type Stack struct {
	// Op is "delete-stack", "execute-change-set" or "deploy".
	Op string `json:"op"`
	// Name is the stack name or ARN.
	Name string `json:"name"`
	// ChangeSet is the change set name or ARN (execute-change-set).
	ChangeSet string `json:"change_set,omitempty"`
	// Retain are the logical ids a delete-stack keeps (--retain-resources).
	Retain []string `json:"retain,omitempty"`
	// DeployArgs are the arguments after "deploy", to make the same change
	// set without running it.
	DeployArgs []string `json:"deploy_args,omitempty"`
}

var stackName = regexp.MustCompile(`^([A-Za-z][-A-Za-z0-9]{0,127}|arn:aws[-a-z]*:cloudformation:[-a-z0-9]+:[0-9]{12}:(stack|changeSet)/[-A-Za-z0-9/]+)$`)

// cloudformation handles "aws cloudformation <operation>". raw are the
// arguments after "aws".
func cloudformation(p parsed, raw []string) []Effect {
	op := p.pos[1]
	unseen := func(detail string) []Effect {
		return []Effect{{Kind: UnseenDelete, Detail: detail, Database: op}}
	}
	switch op {
	case "delete-stack":
		st := &Stack{Op: op, Name: p.value("stack-name"), Retain: listAfter(raw, "--retain-resources")}
		return []Effect{stackEffect(st, st.Name)}
	case "execute-change-set":
		st := &Stack{Op: op, Name: p.value("stack-name"), ChangeSet: p.value("change-set-name")}
		e := stackEffect(st, st.ChangeSet)
		if st.Name == "" && !strings.HasPrefix(st.ChangeSet, "arn:") {
			e.Uncertain = true // AWS needs the stack too; the command will fail
		}
		return []Effect{e}
	case "deploy":
		st := &Stack{Op: op, Name: p.value("stack-name"), DeployArgs: argsAfter(raw, "deploy")}
		if p.has("no-execute-changeset") {
			return nil // only makes a change set; executing it is checked then
		}
		return []Effect{stackEffect(st, st.Name)}
	case "update-stack":
		return unseen("cloudundo cannot see what update-stack replaces or removes. Use `aws cloudformation deploy`, or a change set with execute-change-set: cloudundo checks those")
	case "delete-stack-instances":
		return unseen("deletes the stacks of a stack set, which cloudundo cannot check yet. Delete each stack with `aws cloudformation delete-stack`, which cloudundo protects")
	}
	return nil
}

func stackEffect(st *Stack, name string) Effect {
	e := Effect{Kind: StackChange, Service: "cloudformation", Stack: st}
	if !stackName.MatchString(name) || (st.Name != "" && !stackName.MatchString(st.Name)) {
		e.Uncertain = true
	}
	return e
}

// listAfter returns the words after flag, up to the next --flag
// (--retain-resources A B C).
func listAfter(raw []string, flag string) []string {
	var out []string
	for i := 0; i < len(raw); i++ {
		if raw[i] == flag {
			for j := i + 1; j < len(raw) && !strings.HasPrefix(raw[j], "--"); j++ {
				out = append(out, raw[j])
			}
		} else if v, ok := strings.CutPrefix(raw[i], flag+"="); ok {
			out = append(out, strings.Split(v, ",")...)
		}
	}
	return out
}

// argsAfter returns the arguments after the first word equal to verb.
func argsAfter(raw []string, verb string) []string {
	for i, a := range raw {
		if a == verb {
			return append([]string{}, raw[i+1:]...)
		}
	}
	return nil
}

// destroyTools are deploy tools whose deletes cloudundo cannot see yet,
// and the alternative each block message points to.
var destroyTools = map[string]struct {
	verbs []string
	hint  string
}{
	"cdk":        {[]string{"destroy"}, "cdk destroy deletes CloudFormation stacks that cloudundo cannot resolve from the CDK app. Run `aws cloudformation delete-stack --stack-name <stack>` instead: cloudundo protects that"},
	"aws-cdk":    {[]string{"destroy"}, "cdk destroy deletes CloudFormation stacks that cloudundo cannot resolve from the CDK app. Run `aws cloudformation delete-stack --stack-name <stack>` instead: cloudundo protects that"},
	"sam":        {[]string{"delete"}, "sam delete deletes a CloudFormation stack. Run `aws cloudformation delete-stack --stack-name <stack>` instead: cloudundo protects that"},
	"pulumi":     {[]string{"destroy", "down"}, "cloudundo cannot read Pulumi plans yet, so it cannot back up what pulumi destroy deletes"},
	"terragrunt": {[]string{"destroy"}, "cloudundo cannot read Terragrunt plans yet. Run `terraform destroy` in the module's folder instead: cloudundo protects that"},
}

// launchers run another program: npx cdk destroy, pnpm dlx cdk destroy.
var launchers = map[string]bool{"npx": true, "pnpm": true, "yarn": true, "bunx": true, "bun": true, "dlx": true, "exec": true, "x": true}

// toolDestroy blocks a destroy through a tool cloudundo cannot read, and
// an agent deleting cloudundo's own backups.
func toolDestroy(args []string) []Effect {
	if len(args) > 1 && filepath.Base(args[0]) == "cloudundo" && args[1] == "cleanup" {
		for _, a := range args[2:] {
			if a == "--yes" || a == "-yes" || strings.HasPrefix(a, "--yes=") || strings.HasPrefix(a, "-yes=") {
				return []Effect{{Kind: UnseenDelete, Service: "cloudundo", Database: "cloudundo",
					Detail:  "cloudundo cleanup --yes deletes the backups undo needs. Only the user runs it, in their own terminal",
					Command: strings.Join(args, " ")}}
			}
		}
		return nil
	}
	i := 0
	for i < len(args) && (launchers[filepath.Base(args[i])] || strings.HasPrefix(args[i], "-")) {
		i++
	}
	if i >= len(args) {
		return nil
	}
	tool, ok := destroyTools[filepath.Base(args[i])]
	if !ok {
		return nil
	}
	for _, a := range args[i+1:] {
		for _, v := range tool.verbs {
			if a == v {
				return []Effect{{Kind: UnseenDelete, Service: filepath.Base(args[i]), Detail: tool.hint,
					Database: filepath.Base(args[i]), Command: strings.Join(args, " ")}}
			}
		}
	}
	return nil
}
