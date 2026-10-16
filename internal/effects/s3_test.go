package effects

import (
	"reflect"
	"testing"

	"github.com/cloudundo/cloudundo/internal/awscli"
)

// strip clears fields the table does not care about.
func strip(effs []Effect) []Effect {
	for i := range effs {
		effs[i].Command = ""
		effs[i].Conn = awscli.Conn{}
		effs[i].Service = ""
	}
	return effs
}

func TestAnalyze(t *testing.T) {
	tests := []struct {
		name string
		line string
		want []Effect
	}{
		{
			name: "rm one object",
			line: "aws s3 rm s3://uploads/a/photo.jpg",
			want: []Effect{{Kind: DeleteObjects, Bucket: "uploads", Key: "a/photo.jpg"}},
		},
		{
			name: "rm recursive prefix",
			line: "aws s3 rm s3://uploads/tmp/ --recursive",
			want: []Effect{{Kind: DeleteObjects, Bucket: "uploads", Prefix: "tmp/", Recursive: true}},
		},
		{
			name: "rm recursive whole bucket with global flags",
			line: "aws --profile prod --region us-east-1 s3 rm s3://uploads --recursive --quiet",
			want: []Effect{{Kind: DeleteObjects, Bucket: "uploads", Recursive: true}},
		},
		{
			name: "rm with filters",
			line: `aws s3 rm s3://logs/ --recursive --exclude "*" --include "*.tmp"`,
			want: []Effect{{Kind: DeleteObjects, Bucket: "logs", Recursive: true, Filtered: true}},
		},
		{
			name: "rb plain",
			line: "aws s3 rb s3://old-bucket",
			want: []Effect{{Kind: DeleteBucket, Bucket: "old-bucket"}},
		},
		{
			name: "rb force (the 3 TB case)",
			line: "aws s3 rb s3://user-data --force",
			want: []Effect{{Kind: DeleteBucket, Bucket: "user-data", Force: true}},
		},
		{
			name: "dryrun changes nothing",
			line: "aws s3 rm s3://uploads --recursive --dryrun",
			want: nil,
		},
		{
			name: "read-only commands",
			line: "aws s3 ls s3://uploads && aws s3 cp s3://uploads/a.txt ./a.txt",
			want: nil,
		},
		{
			name: "cp local file overwrites a key",
			line: "aws s3 cp report.pdf s3://docs/reports/2026.pdf",
			want: []Effect{{Kind: OverwriteObjects, Bucket: "docs", Key: "reports/2026.pdf"}},
		},
		{
			name: "cp into a folder takes the source name",
			line: "aws s3 cp ./build/app.js s3://site/static/",
			want: []Effect{{Kind: OverwriteObjects, Bucket: "site", Key: "static/app.js"}},
		},
		{
			name: "cp recursive",
			line: "aws s3 cp ./dist s3://site/assets --recursive",
			want: []Effect{{Kind: OverwriteObjects, Bucket: "site", Prefix: "assets/", Recursive: true}},
		},
		{
			name: "mv between buckets deletes the source",
			line: "aws s3 mv s3://a/x.csv s3://b/archive/",
			want: []Effect{
				{Kind: DeleteObjects, Bucket: "a", Key: "x.csv", Detail: "moved away"},
				{Kind: OverwriteObjects, Bucket: "b", Key: "archive/x.csv"},
			},
		},
		{
			name: "sync up without delete",
			line: "aws s3 sync ./public s3://site",
			want: []Effect{{Kind: OverwriteObjects, Bucket: "site", Recursive: true}},
		},
		{
			name: "sync with delete",
			line: "aws s3 sync ./public s3://site/www --delete",
			want: []Effect{
				{Kind: OverwriteObjects, Bucket: "site", Prefix: "www/", Recursive: true},
				{Kind: DeleteObjects, Bucket: "site", Prefix: "www/", Recursive: true, Detail: "sync --delete removes objects missing from the source"},
			},
		},
		{
			name: "sync down to disk is not S3 data loss",
			line: "aws s3 sync s3://site ./backup --delete",
			want: nil,
		},
		{
			name: "s3api delete-object",
			line: "aws s3api delete-object --bucket uploads --key a/b.png",
			want: []Effect{{Kind: DeleteObjects, Bucket: "uploads", Key: "a/b.png"}},
		},
		{
			name: "s3api delete-objects shorthand",
			line: "aws s3api delete-objects --bucket uploads --delete 'Objects=[{Key=a.txt},{Key=b.txt}],Quiet=true'",
			want: []Effect{{Kind: DeleteObjects, Bucket: "uploads", Keys: []string{"a.txt", "b.txt"}}},
		},
		{
			name: "s3api delete-objects json",
			line: `aws s3api delete-objects --bucket uploads --delete '{"Objects":[{"Key":"x"}]}'`,
			want: []Effect{{Kind: DeleteObjects, Bucket: "uploads", Keys: []string{"x"}}},
		},
		{
			name: "s3api delete-objects from file is uncertain",
			line: "aws s3api delete-objects --bucket uploads --delete file://keys.json",
			want: []Effect{{Kind: DeleteObjects, Bucket: "uploads", Uncertain: true, Detail: "could not read the key list"}},
		},
		{
			name: "s3api delete-bucket",
			line: "aws s3api delete-bucket --bucket old",
			want: []Effect{{Kind: DeleteBucket, Bucket: "old", Detail: "AWS refuses this while the bucket still has objects"}},
		},
		{
			name: "s3api put-object",
			line: "aws s3api put-object --bucket cfg --key app.json --body app.json",
			want: []Effect{{Kind: OverwriteObjects, Bucket: "cfg", Key: "app.json"}},
		},
		{
			name: "suspending versioning weakens undo",
			line: "aws s3api put-bucket-versioning --bucket uploads --versioning-configuration Status=Suspended",
			want: []Effect{{Kind: WeakenProtection, Bucket: "uploads", Detail: "suspends versioning, so later deletes cannot be undone"}},
		},
		{
			name: "enabling versioning is harmless",
			line: "aws s3api put-bucket-versioning --bucket uploads --versioning-configuration Status=Enabled",
			want: nil,
		},
		{
			name: "lifecycle rules can expire data",
			line: "aws s3api put-bucket-lifecycle-configuration --bucket logs --lifecycle-configuration file://lc.json",
			want: []Effect{{Kind: WeakenProtection, Bucket: "logs", Detail: "lifecycle rules can expire objects and old versions"}},
		},
		{
			name: "variable bucket is uncertain",
			line: "aws s3 rm s3://$BUCKET --recursive",
			want: []Effect{{Kind: DeleteObjects, Bucket: "$BUCKET", Recursive: true, Uncertain: true}},
		},
		{
			name: "xargs delete is uncertain with no bucket",
			line: "cat keys | xargs -I{} aws s3 rm s3://uploads/{}",
			want: []Effect{{Kind: DeleteObjects, Bucket: "uploads", Key: "{}", Uncertain: true}},
		},
		{
			name: "delete inside a loop is caught",
			line: `for k in $(aws s3 ls s3://logs | awk '{print $4}'); do aws s3 rm s3://logs/$k; done`,
			want: []Effect{{Kind: DeleteObjects, Bucket: "logs", Key: "$k", Uncertain: true}},
		},
		{
			name: "xargs supplies the target",
			line: "cat keys | xargs aws s3 rm",
			want: []Effect{{Kind: DeleteObjects, Uncertain: true, Detail: "the target is not written in the command"}},
		},
		{
			name: "target in a variable",
			line: "aws s3 rm $TARGET --recursive",
			want: []Effect{{Kind: DeleteObjects, Uncertain: true, Detail: "the target is not written in the command"}},
		},
		{
			name: "bucket delete from a variable",
			line: "aws s3 rb \"$B\" --force",
			want: []Effect{{Kind: DeleteBucket, Uncertain: true, Detail: "the target is not written in the command"}},
		},
		{
			name: "copy to a variable destination",
			line: "aws s3 cp report.pdf $DEST",
			want: []Effect{{Kind: OverwriteObjects, Uncertain: true, Detail: "the target is not written in the command"}},
		},
		{
			name: "copy of a variable file into a folder",
			line: "aws s3 cp $FILE s3://site/static/",
			want: []Effect{{Kind: OverwriteObjects, Bucket: "site", Key: "static/$FILE", Uncertain: true}},
		},
		{
			name: "copy to a variable path could be S3, so it counts",
			line: "aws s3 cp s3://site/a.txt $OUT",
			want: []Effect{{Kind: OverwriteObjects, Uncertain: true, Detail: "the target is not written in the command"}},
		},
		{
			name: "rm of a local path is rejected by aws",
			line: "aws s3 rm ./local.txt",
			want: nil,
		},
		{
			name: "inside bash -c",
			line: `bash -c "aws s3 rb s3://scratch --force"`,
			want: []Effect{{Kind: DeleteBucket, Bucket: "scratch", Force: true}},
		},
		{
			name: "flag=value form",
			line: "aws s3api delete-object --bucket=uploads --key=k1",
			want: []Effect{{Kind: DeleteObjects, Bucket: "uploads", Key: "k1"}},
		},
		{
			name: "access point ARN target is uncertain",
			line: "aws s3 rm s3://arn:aws:s3:us-east-1:123456789012:accesspoint/ap/k",
			want: []Effect{{Kind: DeleteObjects, Uncertain: true, Detail: "no object key"}},
		},
		{
			name: "rm without a key is uncertain",
			line: "aws s3 rm s3://uploads",
			want: []Effect{{Kind: DeleteObjects, Bucket: "uploads", Uncertain: true, Detail: "no object key"}},
		},
		{
			name: "not aws",
			line: "rm -rf ./build && terraform plan",
			want: nil,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := strip(Analyze(tt.line))
			if !reflect.DeepEqual(got, tt.want) {
				t.Fatalf("Analyze(%q)\n got  %+v\n want %+v", tt.line, got, tt.want)
			}
		})
	}
}

func TestCommandIsKept(t *testing.T) {
	effs := Analyze("echo start && aws s3 rm s3://b/k")
	if len(effs) != 1 || effs[0].Command != "aws s3 rm s3://b/k" {
		t.Fatalf("got %+v", effs)
	}
}

func TestConnIsKept(t *testing.T) {
	effs := Analyze("export AWS_PROFILE=prod; AWS_SECRET_ACCESS_KEY=s3cr3t aws --region eu-west-1 s3 rm s3://b/k")
	if len(effs) != 1 {
		t.Fatalf("got %+v", effs)
	}
	c := effs[0].Conn
	if c.Region != "eu-west-1" || c.Env["AWS_PROFILE"] != "prod" || !c.HasInlineCredentials() {
		t.Fatalf("conn = %+v", c)
	}
	if _, ok := c.Env["AWS_SECRET_ACCESS_KEY"]; ok {
		t.Fatal("secret key kept in the journal-visible env")
	}
}
