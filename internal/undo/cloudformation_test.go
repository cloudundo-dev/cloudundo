package undo

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/cloudundo-dev/cloudundo/internal/awscli"
	"github.com/cloudundo-dev/cloudundo/internal/awstest"
	"github.com/cloudundo-dev/cloudundo/internal/effects"
	"github.com/cloudundo-dev/cloudundo/internal/journal"
	"github.com/cloudundo-dev/cloudundo/internal/protect"
)

func (w *world) runCFN(line string) (*journal.Entry, protect.Result) {
	w.t.Helper()
	cfg := protect.Defaults()
	cfg.Now = w.f.Now
	cfg.Sleep = func(time.Duration) {}
	r := protect.Run(context.Background(), effects.Analyze(line), w.cf, cfg)
	return &journal.Entry{ID: 4, Command: line, Undo: r.Undo, Status: "protected"}, r
}

func kindsOf(r protect.Result) map[string]int {
	k := map[string]int{}
	for _, s := range r.Undo {
		k[s.Kind]++
	}
	return k
}

// appStack: a bucket, a table and a database with data, a bucket the
// template keeps, a function (no data), and a table kept by the command.
func appStack(w *world, yaml bool) {
	w.f.CreateBucket("app-uploads", "us-east-1", "")
	w.f.Put("app-uploads", "a.png", "A")
	w.f.CreateBucket("app-logs", "us-east-1", "")
	w.f.Put("app-logs", "l.txt", "L")
	w.f.CreateTable("app-users", "pk")
	w.f.PutItem("app-users", `{"pk":{"S":"u1"}}`)
	w.f.CreateTable("app-audit", "pk")
	w.f.CreateDB(awscli.DBInfo{ID: "app-db", Engine: "postgres", Class: "db.t3.micro", BackupRetention: 1}, "rows")
	w.f.CreateStack("app", []awstest.FakeResource{
		{Logical: "Uploads", Physical: "app-uploads", Type: "AWS::S3::Bucket"},
		{Logical: "Logs", Physical: "app-logs", Type: "AWS::S3::Bucket", Policy: "Retain"},
		{Logical: "Users", Physical: "app-users", Type: "AWS::DynamoDB::Table"},
		{Logical: "Audit", Physical: "app-audit", Type: "AWS::DynamoDB::Table"},
		{Logical: "Db", Physical: "app-db", Type: "AWS::RDS::DBInstance", Policy: "Snapshot"},
		{Logical: "Fn", Physical: "app-fn", Type: "AWS::Lambda::Function"},
		{Logical: "Gone", Physical: "app-old", Type: "AWS::DynamoDB::Table", Status: "DELETE_COMPLETE"},
	}, yaml)
}

func TestCloudFormationDeleteStackIsProtectedAndUndone(t *testing.T) {
	w := newWorld(t)
	appStack(w, false)
	e, r := w.runCFN("aws cloudformation delete-stack --stack-name app --retain-resources Audit")
	if !r.Allowed {
		t.Fatalf("blocked: %v", r.Blocked)
	}
	k := kindsOf(r)
	if k["s3-bucket-copy"] != 1 || k["ddb-table-backup"] != 1 || k["rds-snapshot"] != 1 || len(r.Undo) != 3 {
		t.Fatalf("undo steps: %+v", k)
	}
	notes := strings.Join(r.Notes, "\n")
	if strings.Contains(notes, "app-old") {
		t.Errorf("a resource already deleted was looked at:\n%s", notes)
	}
	for _, want := range []string{"app/Logs", "app/Audit", "app/Fn"} {
		if !strings.Contains(notes, want) {
			t.Errorf("notes lack %s:\n%s", want, notes)
		}
	}

	// CloudFormation deletes them.
	w.f.RemoveBucket("app-uploads")
	w.f.DropTable("app-users")
	w.f.DropDB("app-db", false, "")

	p, err := For(context.Background(), e, w.cf, t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	if _, err := p.Apply(context.Background()); err != nil {
		t.Fatalf("apply: %v", err)
	}
	if v, ok := w.f.Get("app-uploads", "a.png"); !ok || v != "A" {
		t.Fatalf("bucket not back: %q %v", v, ok)
	}
	if _, _, ok := w.f.DB("app-db", false); !ok {
		t.Fatal("database not restored")
	}
	if !w.f.HasTable("app-users") {
		t.Fatal("table not restored")
	}
}

// A YAML template cannot be read, so a resource it keeps is still backed
// up: an extra backup is cheap, a skipped one could lose data.
func TestCloudFormationYAMLTemplateProtectsEverything(t *testing.T) {
	w := newWorld(t)
	appStack(w, true)
	_, r := w.runCFN("aws cloudformation delete-stack --stack-name app")
	if !r.Allowed {
		t.Fatalf("blocked: %v", r.Blocked)
	}
	if k := kindsOf(r); k["s3-bucket-copy"] != 2 || k["ddb-table-backup"] != 2 {
		t.Fatalf("want both buckets and both tables backed up: %+v", k)
	}
}

func TestCloudFormationBlocksDataItCannotBackUp(t *testing.T) {
	w := newWorld(t)
	w.f.CreateStack("disks", []awstest.FakeResource{
		{Logical: "Vol", Physical: "vol-1", Type: "AWS::EC2::Volume"},
	}, false)
	_, r := w.runCFN("aws cloudformation delete-stack --stack-name disks")
	if r.Allowed || !strings.Contains(strings.Join(r.Blocked, " "), "AWS::EC2::Volume") {
		t.Fatalf("an EBS volume must block: %+v", r)
	}

	w.f.CreateStack("snapdisks", []awstest.FakeResource{
		{Logical: "Vol", Physical: "vol-2", Type: "AWS::EC2::Volume", Policy: "Snapshot"},
	}, false)
	_, r = w.runCFN("aws cloudformation delete-stack --stack-name snapdisks")
	if !r.Allowed || !strings.Contains(strings.Join(r.Notes, " "), "DeletionPolicy: Snapshot") {
		t.Fatalf("a volume CloudFormation snapshots may go: %+v", r)
	}
}

func TestCloudFormationFollowsNestedStacks(t *testing.T) {
	w := newWorld(t)
	w.f.CreateTable("child-orders", "pk")
	child := w.f.CreateStack("child", []awstest.FakeResource{
		{Logical: "Orders", Physical: "child-orders", Type: "AWS::DynamoDB::Table"},
	}, false)
	w.f.CreateStack("parent", []awstest.FakeResource{
		{Logical: "Child", Physical: child, Type: "AWS::CloudFormation::Stack"},
	}, false)
	_, r := w.runCFN("aws cloudformation delete-stack --stack-name parent")
	if !r.Allowed || kindsOf(r)["ddb-table-backup"] != 1 {
		t.Fatalf("nested table not protected: %+v", r)
	}
}

func TestCloudFormationChangeSet(t *testing.T) {
	w := newWorld(t)
	w.f.CreateTable("shop-carts", "pk")
	w.f.CreateDB(awscli.DBInfo{ID: "shop-db", Engine: "postgres", Class: "db.t3.micro"}, "rows")
	w.f.CreateDB(awscli.DBInfo{ID: "shop-ro", Engine: "postgres", Class: "db.t3.micro"}, "rows")
	w.f.CreateBucket("shop-assets", "us-east-1", "")
	w.f.CreateStack("shop", []awstest.FakeResource{
		{Logical: "Carts", Physical: "shop-carts", Type: "AWS::DynamoDB::Table"},
		{Logical: "Db", Physical: "shop-db", Type: "AWS::RDS::DBInstance"},
		{Logical: "Ro", Physical: "shop-ro", Type: "AWS::RDS::DBInstance", ReplacePolicy: "Retain"},
		{Logical: "Assets", Physical: "shop-assets", Type: "AWS::S3::Bucket"},
	}, false)
	arn := w.f.AddChangeSet("shop", "v2", []awstest.FakeChange{
		{Action: "Remove", Logical: "Carts", Physical: "shop-carts", Type: "AWS::DynamoDB::Table"},
		{Action: "Modify", Logical: "Db", Physical: "shop-db", Type: "AWS::RDS::DBInstance", Replacement: "True"},
		{Action: "Modify", Logical: "Ro", Physical: "shop-ro", Type: "AWS::RDS::DBInstance", Replacement: "True"},
		{Action: "Modify", Logical: "Assets", Physical: "shop-assets", Type: "AWS::S3::Bucket", Replacement: "False"},
		{Action: "Add", Logical: "New", Type: "AWS::DynamoDB::Table"},
	})
	for _, line := range []string{
		"aws cloudformation execute-change-set --change-set-name v2 --stack-name shop",
		"aws cloudformation execute-change-set --change-set-name " + arn,
	} {
		_, r := w.runCFN(line)
		if !r.Allowed {
			t.Fatalf("%s blocked: %v", line, r.Blocked)
		}
		k := kindsOf(r)
		if k["ddb-table-backup"] != 1 || k["rds-snapshot"] != 1 || len(r.Undo) != 2 {
			t.Fatalf("%s: want the removed table and the replaced database: %+v", line, k)
		}
		if !strings.Contains(strings.Join(r.Notes, " "), "shop/Ro") {
			t.Fatalf("the database kept on replacement should be named: %v", r.Notes)
		}
	}

	w.f.SetChangeSetMore(arn)
	if _, r := w.runCFN("aws cloudformation execute-change-set --change-set-name " + arn); r.Allowed {
		t.Fatal("a change set with more pages must block")
	}
	if _, r := w.runCFN("aws cloudformation execute-change-set --change-set-name v2"); r.Allowed {
		t.Fatal("a change set name without its stack cannot be read and must block")
	}
}

func TestCloudFormationDeployReadsItsChangeSetFirst(t *testing.T) {
	w := newWorld(t)
	w.f.CreateTable("api-keys", "pk")
	w.f.CreateStack("api", []awstest.FakeResource{
		{Logical: "Keys", Physical: "api-keys", Type: "AWS::DynamoDB::Table"},
	}, false)
	line := "aws cloudformation deploy --template-file t.yaml --stack-name api --capabilities CAPABILITY_IAM"

	// Nothing to change.
	_, r := w.runCFN(line)
	if !r.Allowed || len(r.Undo) != 0 || !strings.Contains(strings.Join(r.Notes, " "), "changes nothing") {
		t.Fatalf("no-op deploy: %+v", r)
	}

	// The new template drops the table.
	w.f.SetDeployChanges("api", []awstest.FakeChange{
		{Action: "Remove", Logical: "Keys", Physical: "api-keys", Type: "AWS::DynamoDB::Table"},
	})
	_, r = w.runCFN(line)
	if !r.Allowed || kindsOf(r)["ddb-table-backup"] != 1 {
		t.Fatalf("deploy that removes a table: %+v", r)
	}
	var made string
	for _, c := range w.f.Calls {
		j := strings.Join(c, " ")
		if strings.Contains(j, "cloudformation deploy") {
			if !strings.Contains(j, "--no-execute-changeset") || !strings.Contains(j, "--capabilities CAPABILITY_IAM") {
				t.Fatalf("cloudundo must only make the change set, with the same options: %s", j)
			}
		}
		if strings.Contains(j, "delete-change-set") {
			made = j
		}
	}
	if made == "" {
		t.Fatal("the change set cloudundo made was not deleted")
	}

	// A stack that does not exist yet only gets created.
	_, r = w.runCFN("aws cloudformation deploy --template-file t.yaml --stack-name brand-new")
	if !r.Allowed || len(r.Undo) != 0 {
		t.Fatalf("new stack: %+v", r)
	}
}

func TestCloudFormationUnclearOrMissing(t *testing.T) {
	w := newWorld(t)
	if _, r := w.runCFN("aws cloudformation delete-stack --stack-name $STACK"); r.Allowed {
		t.Fatal("a stack name in a variable must block")
	}
	_, r := w.runCFN("aws cloudformation delete-stack --stack-name nope")
	if !r.Allowed || !strings.Contains(strings.Join(r.Notes, " "), "does not exist") {
		t.Fatalf("missing stack: %+v", r)
	}
	if _, r := w.runCFN("aws cloudformation update-stack --stack-name app --template-body file://t.json"); r.Allowed {
		t.Fatal("update-stack must block")
	}
}
