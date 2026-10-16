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
			default:
				t.Fatalf("effect without service for %q: %+v", line, e)
			}
		}
	})
}
