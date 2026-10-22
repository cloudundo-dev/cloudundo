package effects

import "testing"

// FuzzAnalyze makes sure no command line, however malformed, can crash
// the parser. A crash in a hook would let the agent's command through.
func FuzzAnalyze(f *testing.F) {
	seeds := []string{
		"aws s3 rm s3://b/k --recursive",
		`bash -c "aws s3 rb s3://b --force"`,
		"for k in $(cat x); do aws s3 rm s3://b/$k; done",
		"echo `aws s3 rb s3://c`",
		`aws s3api delete-objects --bucket b --delete '{"Objects":[{"Key":"x"}]}'`,
		"aws s3 rm 's3://unterminated",
		`aws s3 rm "s3://b/$(`,
		"aws s3 cp a s3:// > 2>&",
		"sudo -u",
		"bash -c",
		"\\",
		"$((((",
		"aws --profile",
		"aws s3 mv s3://a/ s3://",
		"aws dynamodb delete-table --table-name T",
		`aws dynamodb batch-write-item --request-items '{"T":[{"DeleteRequest":{"Key":{"pk":{"S":"a"}}}}]}'`,
		"aws dynamodb delete-item --table-name $T --key",
		`aws dynamodb execute-statement --statement "DELETE FROM T"`,
		"aws rds delete-db-instance --db-instance-identifier d --skip-final-snapshot",
		"aws rds delete-db-cluster --db-cluster-identifier c --final-db-snapshot-identifier f",
		"aws elasticache delete-replication-group --replication-group-id g",
		"redis-cli -h x FLUSHALL",
		"cd infra && terraform destroy -auto-approve -target=aws_db_instance.main",
		"terraform -chdir=x apply plan.out",
		"aws cloudformation delete-stack --stack-name app --retain-resources A B",
		"aws cloudformation execute-change-set --change-set-name v2 --stack-name app",
		"aws cloudformation deploy --template-file t.yaml --stack-name app",
		"aws cloudformation update-stack --stack-name app",
		"npx cdk destroy --force",
		"terragrunt run --all -- destroy",
	}
	for _, s := range seeds {
		f.Add(s)
	}
	f.Fuzz(func(t *testing.T, line string) {
		for _, e := range Analyze(line) {
			if e.Kind == "" {
				t.Fatalf("effect without kind for %q", line)
			}
			switch e.Service {
			case "s3":
				if e.Bucket == "" && !e.Uncertain {
					t.Fatalf("unknown bucket not marked uncertain for %q: %+v", line, e)
				}
			case "dynamodb":
				if (e.Table == "" || e.Table == "?") && !e.Uncertain {
					t.Fatalf("unknown table not marked uncertain for %q: %+v", line, e)
				}
				if (e.Kind == DeleteItems || e.Kind == OverwriteItems) && len(e.Items) == 0 && !e.Uncertain {
					t.Fatalf("item change without items not marked uncertain for %q: %+v", line, e)
				}
			case "rds":
				if !validName(e.Database) && !e.Uncertain {
					t.Fatalf("unknown database not marked uncertain for %q: %+v", line, e)
				}
			case "elasticache":
				if !validName(e.Cache) && !e.Uncertain {
					t.Fatalf("unknown cache not marked uncertain for %q: %+v", line, e)
				}
			case "terraform":
				if e.TF == nil || e.Kind != TerraformChange {
					t.Fatalf("terraform effect %+v", e)
				}
			case "redis":
				if e.Kind != EraseCacheData {
					t.Fatalf("redis effect %+v", e)
				}
			case "cloudformation":
				switch e.Kind {
				case StackChange:
					if e.Stack == nil {
						t.Fatalf("stack effect without stack for %q", line)
					}
					name := e.Stack.Name
					if e.Stack.Op == "execute-change-set" {
						name = e.Stack.ChangeSet
					}
					if !stackName.MatchString(name) && !e.Uncertain {
						t.Fatalf("unknown stack not marked uncertain for %q: %+v", line, e)
					}
				case UnseenDelete:
				default:
					t.Fatalf("cloudformation effect %+v", e)
				}
			default:
				if e.Kind == UnseenDelete && e.Detail != "" {
					continue // a destroy through cdk, sam, pulumi or terragrunt: blocked
				}
				t.Fatalf("effect without service for %q: %+v", line, e)
			}
		}
	})
}
