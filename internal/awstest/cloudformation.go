package awstest

import (
	"encoding/json"
	"fmt"

	"github.com/cloudundo-dev/cloudundo/internal/awscli"
)

// FakeResource is one resource of a fake CloudFormation stack.
type FakeResource struct {
	Logical, Physical, Type string
	Status                  string // default CREATE_COMPLETE
	// Policy and ReplacePolicy end up in the stack's JSON template.
	Policy, ReplacePolicy string
}

// FakeChange is one resource change of a fake change set.
type FakeChange struct {
	Action, Logical, Physical, Type, Replacement, ChangeSetID string
}

type fstack struct {
	id        string
	status    string
	resources []FakeResource
	yaml      bool
}

type fchangeSet struct {
	stack   string
	changes []FakeChange
	exec    string
	more    bool
}

// CreateStack adds a stack. With yamlTemplate its template is YAML text,
// which cloudundo cannot read policies from.
func (f *Fake) CreateStack(name string, resources []FakeResource, yamlTemplate bool) string {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.cfnInit()
	id := fmt.Sprintf("arn:aws:cloudformation:us-east-1:%s:stack/%s/id-%d", f.Account, name, len(f.stacks)+1)
	f.stacks[name] = &fstack{id: id, status: "CREATE_COMPLETE", resources: resources, yaml: yamlTemplate}
	return id
}

// AddChangeSet adds a change set to a stack and returns its ARN.
func (f *Fake) AddChangeSet(stack, name string, changes []FakeChange) string {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.cfnInit()
	return f.addChangeSet(stack, name, changes)
}

func (f *Fake) addChangeSet(stack, name string, changes []FakeChange) string {
	arn := fmt.Sprintf("arn:aws:cloudformation:us-east-1:%s:changeSet/%s/cs-%d", f.Account, name, len(f.changeSets)+1)
	cs := &fchangeSet{stack: stack, changes: changes, exec: "AVAILABLE"}
	f.changeSets[arn] = cs
	f.changeSets[stack+"|"+name] = cs
	return arn
}

// SetChangeSetMore marks a change set as having more pages of changes.
func (f *Fake) SetChangeSetMore(arn string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.changeSets[arn].more = true
}

// ChangeSetExists reports whether a change set is still there.
func (f *Fake) ChangeSetExists(arn string) bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	_, ok := f.changeSets[arn]
	return ok
}

// SetDeployChanges sets what the next `cloudformation deploy` of a stack
// would change.
func (f *Fake) SetDeployChanges(stack string, changes []FakeChange) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.cfnInit()
	f.deployChanges[stack] = changes
}

func (f *Fake) cfnInit() {
	if f.stacks == nil {
		f.stacks = map[string]*fstack{}
		f.changeSets = map[string]*fchangeSet{}
		f.deployChanges = map[string][]FakeChange{}
	}
}

func (f *Fake) stack(ref string) *fstack {
	if s := f.stacks[ref]; s != nil {
		return s
	}
	for _, s := range f.stacks {
		if s.id == ref {
			return s
		}
	}
	return nil
}

func (f *Fake) cfnCall(op string, flags map[string]string, args []string) ([]byte, error) {
	f.cfnInit()
	fail := func(msg string) ([]byte, error) { return f.fail(args, msg) }
	notFound := func(name string) ([]byte, error) {
		return fail(fmt.Sprintf("An error occurred (ValidationError) when calling the operation: Stack with id %s does not exist", name))
	}
	js := func(v any) ([]byte, error) { return json.Marshal(v) }
	switch op {
	case "describe-stacks":
		s := f.stack(flags["stack-name"])
		if s == nil {
			return notFound(flags["stack-name"])
		}
		return js(map[string]any{"Stacks": []map[string]string{{"StackId": s.id, "StackStatus": s.status}}})

	case "list-stack-resources":
		s := f.stack(flags["stack-name"])
		if s == nil {
			return notFound(flags["stack-name"])
		}
		var out []map[string]string
		for _, r := range s.resources {
			st := r.Status
			if st == "" {
				st = "CREATE_COMPLETE"
			}
			out = append(out, map[string]string{"LogicalResourceId": r.Logical, "PhysicalResourceId": r.Physical,
				"ResourceType": r.Type, "ResourceStatus": st})
		}
		return js(map[string]any{"StackResourceSummaries": out})

	case "get-template":
		s := f.stack(flags["stack-name"])
		if s == nil {
			return notFound(flags["stack-name"])
		}
		if s.yaml {
			return js(map[string]any{"TemplateBody": "Resources:\n  Data:\n    Type: AWS::S3::Bucket\n    DeletionPolicy: Retain\n"})
		}
		res := map[string]map[string]string{}
		for _, r := range s.resources {
			m := map[string]string{"Type": r.Type}
			if r.Policy != "" {
				m["DeletionPolicy"] = r.Policy
			}
			if r.ReplacePolicy != "" {
				m["UpdateReplacePolicy"] = r.ReplacePolicy
			}
			res[r.Logical] = m
		}
		return js(map[string]any{"TemplateBody": map[string]any{"Resources": res}})

	case "describe-change-set":
		name := flags["change-set-name"]
		cs := f.changeSets[name]
		if cs == nil {
			cs = f.changeSets[flags["stack-name"]+"|"+name]
		}
		if cs == nil {
			return fail("An error occurred (ChangeSetNotFound) when calling the operation: ChangeSet " + name + " does not exist")
		}
		var changes []map[string]any
		for _, ch := range cs.changes {
			changes = append(changes, map[string]any{"Type": "Resource", "ResourceChange": map[string]string{
				"Action": ch.Action, "LogicalResourceId": ch.Logical, "PhysicalResourceId": ch.Physical,
				"ResourceType": ch.Type, "Replacement": ch.Replacement, "ChangeSetId": ch.ChangeSetID}})
		}
		s := f.stack(cs.stack)
		id := ""
		if s != nil {
			id = s.id
		}
		out := map[string]any{"StackId": id, "StackName": cs.stack, "Status": "CREATE_COMPLETE",
			"ExecutionStatus": cs.exec, "Changes": changes}
		if cs.more {
			out["NextToken"] = "more"
		}
		return js(out)

	case "delete-change-set":
		name := flags["change-set-name"]
		cs := f.changeSets[name]
		if cs == nil {
			return fail("An error occurred (ChangeSetNotFound) when calling the operation: ChangeSet " + name + " does not exist")
		}
		for k, v := range f.changeSets {
			if v == cs {
				delete(f.changeSets, k)
			}
		}
		return js(map[string]any{})

	case "deploy":
		stack := flags["stack-name"]
		if flags["no-execute-changeset"] == "" {
			return fail("the fake only makes change sets; it does not deploy")
		}
		changes := f.deployChanges[stack]
		if len(changes) == 0 {
			return nil, f.cliExit(args, 255, "\nNo changes to deploy. Stack "+stack+" is up to date\n")
		}
		f.nextID++
		arn := f.addChangeSet(stack, fmt.Sprintf("awscli-cloudformation-package-deploy-%d", f.nextID), changes)
		return []byte("\nWaiting for changeset to be created..\nChangeset created successfully. Run the following command to review changes:\naws cloudformation describe-change-set --change-set-name " + arn + "\n"), nil
	}
	return fail("fake: unsupported cloudformation " + op)
}

func (f *Fake) fail(args []string, msg string) ([]byte, error) {
	return nil, &awscli.Error{Args: args, Code: 254, Stderr: msg}
}

func (f *Fake) cliExit(args []string, code int, stderr string) error {
	return &awscli.Error{Args: args, Code: code, Stderr: stderr}
}
