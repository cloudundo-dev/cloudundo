package shellparse

import (
	"reflect"
	"testing"
)

func TestSplit(t *testing.T) {
	tests := []struct {
		name      string
		line      string
		want      [][]string
		uncertain []bool
	}{
		{
			name: "single command",
			line: "aws s3 rm s3://b/k",
			want: [][]string{{"aws", "s3", "rm", "s3://b/k"}},
		},
		{
			name: "chained with && ; || and pipe",
			line: "echo hi && aws s3 ls; aws s3 rb s3://b || true | cat",
			want: [][]string{{"echo", "hi"}, {"aws", "s3", "ls"}, {"aws", "s3", "rb", "s3://b"}, {"true"}, {"cat"}},
		},
		{
			name: "quotes keep spaces",
			line: `aws s3 rm "s3://b/my file.txt" 's3://b/x y'`,
			want: [][]string{{"aws", "s3", "rm", "s3://b/my file.txt", "s3://b/x y"}},
		},
		{
			name: "env assignments and sudo are stripped",
			line: "AWS_PROFILE=prod sudo -u admin aws s3 rm s3://b/k",
			want: [][]string{{"aws", "s3", "rm", "s3://b/k"}},
		},
		{
			name: "env wrapper and timeout",
			line: "env -i AWS_REGION=us-east-1 timeout 30 aws s3 rb s3://b",
			want: [][]string{{"aws", "s3", "rb", "s3://b"}},
		},
		{
			name: "bash -c is followed",
			line: `bash -lc "cd app && aws s3 rm s3://b/k --recursive"`,
			want: [][]string{{"cd", "app"}, {"aws", "s3", "rm", "s3://b/k", "--recursive"}},
		},
		{
			name: "redirections are dropped",
			line: "aws s3 rm s3://b/k > out.log 2>&1",
			want: [][]string{{"aws", "s3", "rm", "s3://b/k"}},
		},
		{
			name:      "variables make it uncertain",
			line:      "aws s3 rm s3://$BUCKET/k",
			want:      [][]string{{"aws", "s3", "rm", "s3://$BUCKET/k"}},
			uncertain: []bool{true},
		},
		{
			name:      "command substitution makes it uncertain",
			line:      "aws s3 rb s3://$(cat bucket.txt) --force",
			want:      [][]string{{"aws", "s3", "rb", "s3://$(cat bucket.txt)", "--force"}, {"cat", "bucket.txt"}},
			uncertain: []bool{true, false},
		},
		{
			name:      "xargs hides arguments",
			line:      "cat keys.txt | xargs -n1 aws s3 rm",
			want:      [][]string{{"cat", "keys.txt"}, {"aws", "s3", "rm"}},
			uncertain: []bool{false, true},
		},
		{
			name: "line continuation and comments",
			line: "aws s3 rm \\\n  s3://b/k # clean up",
			want: [][]string{{"aws", "s3", "rm", "s3://b/k"}},
		},
		{
			name: "absolute path to aws",
			line: "/usr/local/bin/aws s3 rm s3://b/k",
			want: [][]string{{"/usr/local/bin/aws", "s3", "rm", "s3://b/k"}},
		},
		{
			name:      "commands inside a for loop",
			line:      `for k in $(cat keys); do aws s3 rm s3://logs/$k; done`,
			want:      [][]string{{"for", "k", "in", "$(cat keys)"}, {"aws", "s3", "rm", "s3://logs/$k"}, {"cat", "keys"}},
			uncertain: []bool{true, true, false},
		},
		{
			name:      "command substitution runs too",
			line:      `echo "done: $(aws s3 rm s3://b --recursive)"; x=` + "`aws s3 rb s3://c --force`",
			want:      [][]string{{"echo", "done: $(aws s3 rm s3://b --recursive)"}, {"aws", "s3", "rm", "s3://b", "--recursive"}, {"aws", "s3", "rb", "s3://c", "--force"}},
			uncertain: []bool{true, false, false},
		},
		{
			name: "if and while conditions",
			line: `if aws s3 ls s3://b; then aws s3 rb s3://b --force; fi; while true; do aws s3 rm s3://b/k; done`,
			want: [][]string{
				{"aws", "s3", "ls", "s3://b"}, {"aws", "s3", "rb", "s3://b", "--force"},
				{"true"}, {"aws", "s3", "rm", "s3://b/k"},
			},
		},
		{
			name: "single quotes keep dollar literal",
			line: `aws s3 rm 's3://b/$literal'`,
			want: [][]string{{"aws", "s3", "rm", "s3://b/$literal"}},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := Split(tt.line)
			var args [][]string
			for _, s := range got {
				args = append(args, s.Args)
			}
			if !reflect.DeepEqual(args, tt.want) {
				t.Fatalf("Split(%q)\n got  %q\n want %q", tt.line, args, tt.want)
			}
			for i, s := range got {
				want := false
				if i < len(tt.uncertain) {
					want = tt.uncertain[i]
				}
				if s.Uncertain != want {
					t.Errorf("command %d %q: Uncertain = %v, want %v", i, s.Args, s.Uncertain, want)
				}
			}
		})
	}
}
