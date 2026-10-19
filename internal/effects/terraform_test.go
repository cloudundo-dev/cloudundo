package effects

import (
	"reflect"
	"testing"
)

func TestAnalyzeTerraform(t *testing.T) {
	tests := []struct {
		line string
		want *Terraform
		unc  bool
	}{
		{"terraform destroy -auto-approve", &Terraform{Bin: "terraform", Destroy: true}, false},
		{"cd infra/prod && terraform apply -auto-approve -var-file=prod.tfvars -target aws_db_instance.main",
			&Terraform{Bin: "terraform", Dir: "infra/prod", PlanArgs: []string{"-var-file=prod.tfvars", "-target=aws_db_instance.main"}}, false},
		{"terraform -chdir=stack apply tfplan", &Terraform{Bin: "terraform", Dir: "stack", PlanFile: "tfplan"}, false},
		{"tofu apply -destroy -var 'env=dev'", &Terraform{Bin: "tofu", Destroy: true, PlanArgs: []string{"-var=env=dev"}}, false},
		{"cd /srv/tf; cd modules && terraform destroy", &Terraform{Bin: "terraform", Dir: "/srv/tf/modules", Destroy: true}, false},
		{"cd $TF_DIR && terraform destroy", &Terraform{Bin: "terraform", Destroy: true}, true},
		{"terraform destroy -target=$X", &Terraform{Bin: "terraform", Destroy: true, PlanArgs: []string{"-target=$X"}}, true},
		{"terraform apply -state=old.tfstate", &Terraform{Bin: "terraform"}, true},
	}
	for _, tt := range tests {
		effs := Analyze(tt.line)
		if len(effs) != 1 || effs[0].Kind != TerraformChange || effs[0].Service != "terraform" {
			t.Fatalf("%s: %+v", tt.line, effs)
		}
		if !reflect.DeepEqual(effs[0].TF, tt.want) || effs[0].Uncertain != tt.unc {
			t.Errorf("%s:\n got  %+v uncertain=%v\n want %+v uncertain=%v", tt.line, effs[0].TF, effs[0].Uncertain, tt.want, tt.unc)
		}
	}
	for _, line := range []string{"terraform plan", "terraform init && terraform validate", "terraform show -json", "terraform -version"} {
		if effs := Analyze(line); len(effs) != 0 {
			t.Errorf("%s: %+v", line, effs)
		}
	}
}

func TestTerraformKeepsEnv(t *testing.T) {
	effs := Analyze("AWS_PROFILE=prod terraform destroy")
	if len(effs) != 1 || effs[0].Conn.Env["AWS_PROFILE"] != "prod" {
		t.Fatalf("%+v", effs)
	}
}
