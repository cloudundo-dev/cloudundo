package awscli

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"regexp"
	"strings"
)

// StackResource is one resource of a CloudFormation stack.
type StackResource struct {
	Logical  string
	Physical string
	Type     string
	Status   string
}

// StackResources lists a stack's resources; ok is false when the stack
// does not exist.
func (c Client) StackResources(ctx context.Context, stack string) ([]StackResource, bool, error) {
	out, err := c.call(ctx, "cloudformation", "list-stack-resources", "--stack-name", stack)
	if Has(err, "does not exist") {
		return nil, false, nil
	}
	if err != nil {
		return nil, false, err
	}
	var r struct {
		StackResourceSummaries []struct {
			LogicalResourceId  string
			PhysicalResourceId string
			ResourceType       string
			ResourceStatus     string
		}
	}
	if err := json.Unmarshal(out, &r); err != nil {
		return nil, false, fmt.Errorf("read the resources of stack %s: %w", stack, err)
	}
	var res []StackResource
	for _, s := range r.StackResourceSummaries {
		res = append(res, StackResource{Logical: s.LogicalResourceId, Physical: s.PhysicalResourceId,
			Type: s.ResourceType, Status: s.ResourceStatus})
	}
	return res, true, nil
}

// StackStatus returns a stack's status; ok is false when it does not exist.
func (c Client) StackStatus(ctx context.Context, stack string) (string, bool, error) {
	out, err := c.call(ctx, "cloudformation", "describe-stacks", "--stack-name", stack)
	if Has(err, "does not exist") {
		return "", false, nil
	}
	if err != nil {
		return "", false, err
	}
	var r struct {
		Stacks []struct{ StackStatus string }
	}
	if err := json.Unmarshal(out, &r); err != nil {
		return "", false, fmt.Errorf("read stack %s: %w", stack, err)
	}
	if len(r.Stacks) == 0 {
		return "", false, nil
	}
	return r.Stacks[0].StackStatus, true, nil
}

// StackPolicy is what a template says happens to a resource's data when
// CloudFormation deletes or replaces it: "Delete", "Retain",
// "RetainExceptOnCreate", "Snapshot", or "" when not known.
type StackPolicy struct {
	Deletion, UpdateReplace string
}

// StackPolicies reads DeletionPolicy and UpdateReplacePolicy from a
// stack's template. ok is false when the template cannot be read as JSON
// (YAML templates); callers then assume resources are deleted.
func (c Client) StackPolicies(ctx context.Context, stack string) (map[string]StackPolicy, bool, error) {
	out, err := c.call(ctx, "cloudformation", "get-template", "--stack-name", stack, "--template-stage", "Original")
	if err != nil {
		return nil, false, err
	}
	var r struct{ TemplateBody json.RawMessage }
	if err := json.Unmarshal(out, &r); err != nil {
		return nil, false, fmt.Errorf("read the template of stack %s: %w", stack, err)
	}
	body := r.TemplateBody
	var asString string
	if json.Unmarshal(body, &asString) == nil {
		body = json.RawMessage(asString) // a JSON template sent as text; YAML fails below
	}
	var t struct {
		Resources map[string]struct {
			DeletionPolicy      json.RawMessage
			UpdateReplacePolicy json.RawMessage
		}
	}
	if json.Unmarshal(body, &t) != nil || t.Resources == nil {
		return nil, false, nil
	}
	pol := map[string]StackPolicy{}
	for id, res := range t.Resources {
		var p StackPolicy
		json.Unmarshal(res.DeletionPolicy, &p.Deletion) // a condition (Fn::If) stays "": unknown
		json.Unmarshal(res.UpdateReplacePolicy, &p.UpdateReplace)
		pol[id] = p
	}
	return pol, true, nil
}

// StackChange is one resource change in a change set.
type StackChange struct {
	Action      string // Add, Modify, Remove, Import, Dynamic
	Logical     string
	Physical    string
	Type        string
	Replacement string // True, False, Conditional
	ChangeSetID string // a nested stack's own change set
}

// ChangeSetInfo is what a change set will do.
type ChangeSetInfo struct {
	StackID         string
	StackName       string
	Status          string
	ExecutionStatus string
	Changes         []StackChange
	More            bool // more changes than one page
}

// ChangeSet describes a change set by name (with its stack) or ARN.
func (c Client) ChangeSet(ctx context.Context, name, stack string) (ChangeSetInfo, error) {
	args := []string{"cloudformation", "describe-change-set", "--change-set-name", name}
	if stack != "" {
		args = append(args, "--stack-name", stack)
	}
	out, err := c.call(ctx, args...)
	if err != nil {
		return ChangeSetInfo{}, err
	}
	var r struct {
		StackId, StackName, Status, ExecutionStatus, NextToken string
		Changes                                                []struct {
			ResourceChange struct {
				Action, LogicalResourceId, PhysicalResourceId, ResourceType, Replacement, ChangeSetId string
			}
		}
	}
	if err := json.Unmarshal(out, &r); err != nil {
		return ChangeSetInfo{}, fmt.Errorf("read change set %s: %w", name, err)
	}
	info := ChangeSetInfo{StackID: r.StackId, StackName: r.StackName, Status: r.Status,
		ExecutionStatus: r.ExecutionStatus, More: r.NextToken != ""}
	for _, ch := range r.Changes {
		rc := ch.ResourceChange
		info.Changes = append(info.Changes, StackChange{Action: rc.Action, Logical: rc.LogicalResourceId,
			Physical: rc.PhysicalResourceId, Type: rc.ResourceType, Replacement: rc.Replacement, ChangeSetID: rc.ChangeSetId})
	}
	return info, nil
}

// DeleteChangeSet removes a change set that was only made to be read.
func (c Client) DeleteChangeSet(ctx context.Context, arn string) error {
	_, err := c.call(ctx, "cloudformation", "delete-change-set", "--change-set-name", arn)
	return err
}

var changeSetARN = regexp.MustCompile(`--change-set-name\s+(arn:aws[-a-z]*:cloudformation:\S+)`)

// DeployChangeSet runs a `cloudformation deploy` with the same arguments
// and --no-execute-changeset, which makes the change set without running
// it. It returns the change set's ARN, or noChanges when the deploy would
// change nothing.
func (c Client) DeployChangeSet(ctx context.Context, deployArgs []string) (arn string, noChanges bool, err error) {
	args := append([]string{"cloudformation", "deploy"}, deployArgs...)
	args = append(args, "--no-execute-changeset")
	out, err := c.call(ctx, args...)
	text := string(out)
	var ae *Error
	if err != nil && errors.As(err, &ae) {
		text += ae.Stderr
	}
	if strings.Contains(text, "No changes to deploy") {
		return "", true, nil
	}
	if err != nil {
		return "", false, err
	}
	m := changeSetARN.FindStringSubmatch(text)
	if m == nil {
		return "", false, fmt.Errorf("could not find the change set in the output of cloudformation deploy: %q", strings.TrimSpace(text))
	}
	return m[1], false, nil
}
