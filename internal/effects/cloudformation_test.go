package effects

import (
	"reflect"
	"strings"
	"testing"
)

func TestCloudFormationCommands(t *testing.T) {
	cases := []struct {
		line      string
		op, name  string
		uncertain bool
	}{
		{"aws cloudformation delete-stack --stack-name app", "delete-stack", "app", false},
		{"aws --region eu-west-1 cloudformation delete-stack --stack-name=app", "delete-stack", "app", false},
		{"aws cloudformation delete-stack --stack-name arn:aws:cloudformation:eu-west-1:123456789012:stack/app/abc-123", "delete-stack",
			"arn:aws:cloudformation:eu-west-1:123456789012:stack/app/abc-123", false},
		{"aws cloudformation delete-stack --stack-name $STACK", "delete-stack", "$STACK", true},
		{"aws cloudformation delete-stack", "delete-stack", "", true},
		{"aws cloudformation execute-change-set --change-set-name v2 --stack-name app", "execute-change-set", "app", false},
		{"aws cloudformation execute-change-set --change-set-name v2", "execute-change-set", "", true},
		{"aws cloudformation deploy --template-file t.yaml --stack-name app", "deploy", "app", false},
	}
	for _, c := range cases {
		effs := Analyze(c.line)
		if len(effs) != 1 || effs[0].Kind != StackChange || effs[0].Stack == nil {
			t.Errorf("%q: %+v", c.line, effs)
			continue
		}
		e := effs[0]
		if e.Stack.Op != c.op || e.Stack.Name != c.name || e.Uncertain != c.uncertain {
			t.Errorf("%q: op %q name %q uncertain %v", c.line, e.Stack.Op, e.Stack.Name, e.Uncertain)
		}
	}

	e := Analyze("aws cloudformation delete-stack --stack-name app --retain-resources Logs Users --region eu-west-1")[0]
	if !reflect.DeepEqual(e.Stack.Retain, []string{"Logs", "Users"}) || e.Conn.Region != "eu-west-1" {
		t.Errorf("retain %v region %q", e.Stack.Retain, e.Conn.Region)
	}
	e = Analyze("aws cloudformation deploy --template-file t.yaml --stack-name app --parameter-overrides A=1 B=2")[0]
	if strings.Join(e.Stack.DeployArgs, " ") != "--template-file t.yaml --stack-name app --parameter-overrides A=1 B=2" {
		t.Errorf("deploy args %q", e.Stack.DeployArgs)
	}

	for _, line := range []string{
		"aws cloudformation deploy --template-file t.yaml --stack-name app --no-execute-changeset",
		"aws cloudformation describe-stacks --stack-name app",
		"aws cloudformation create-stack --stack-name app --template-body file://t.json",
		"aws cloudformation create-change-set --stack-name app --change-set-name v2",
	} {
		if effs := Analyze(line); len(effs) != 0 {
			t.Errorf("%q changes no data: %+v", line, effs)
		}
	}
	for _, line := range []string{
		"aws cloudformation update-stack --stack-name app --template-body file://t.json",
		"aws cloudformation delete-stack-instances --stack-set-name s --regions eu-west-1 --accounts 1 --no-retain-stacks",
	} {
		effs := Analyze(line)
		if len(effs) != 1 || effs[0].Kind != UnseenDelete {
			t.Errorf("%q must be an unseen delete: %+v", line, effs)
		}
	}
}

func TestDestroyThroughOtherTools(t *testing.T) {
	for _, line := range []string{
		"cdk destroy --force",
		"npx cdk destroy MyStack",
		"npx aws-cdk destroy",
		"yarn cdk destroy --all",
		"pnpm exec cdk destroy",
		"sam delete --no-prompts",
		"pulumi destroy --yes",
		"pulumi down -y",
		"terragrunt destroy",
		"terragrunt run-all destroy",
		"terragrunt run --all -- destroy",
		"cd infra && npx cdk destroy",
	} {
		effs := Analyze(line)
		if len(effs) != 1 || effs[0].Kind != UnseenDelete || effs[0].Detail == "" {
			t.Errorf("%q must be blocked as an unseen delete: %+v", line, effs)
		}
	}
	for _, line := range []string{
		"cdk deploy", "npx cdk synth", "sam deploy", "pulumi up", "terragrunt plan", "npm run build",
	} {
		if effs := Analyze(line); len(effs) != 0 {
			t.Errorf("%q is not a destroy: %+v", line, effs)
		}
	}
}
