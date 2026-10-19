package policy_test

import (
	"context"
	"encoding/json"
	"path"
	"strings"
	"testing"
	"time"

	"github.com/cloudundo/cloudundo/internal/awscli"
	"github.com/cloudundo/cloudundo/internal/awstest"
	"github.com/cloudundo/cloudundo/internal/effects"
	"github.com/cloudundo/cloudundo/internal/journal"
	"github.com/cloudundo/cloudundo/internal/policy"
	"github.com/cloudundo/cloudundo/internal/protect"
	"github.com/cloudundo/cloudundo/internal/undo"
)

func TestOnlyDenies(t *testing.T) {
	for _, o := range []policy.Options{{}, {Account: "123456789012", Region: "eu-west-1", KeepVersions: true}} {
		d := policy.Build(o)
		if d.Version != "2012-10-17" || len(d.Statement) == 0 {
			t.Fatalf("doc = %+v", d)
		}
		for _, s := range d.Statement {
			if s.Effect != "Deny" {
				t.Fatalf("%s is %s", s.Sid, s.Effect)
			}
			for _, a := range s.Action {
				if strings.HasSuffix(a, ":*") || a == "*" {
					t.Fatalf("%s denies everything: %s", s.Sid, a)
				}
			}
		}
		var back policy.Document
		if err := json.Unmarshal([]byte(d.JSON()), &back); err != nil {
			t.Fatal(err)
		}
		// Inline user policies are limited to 2048 characters without spaces.
		compact, _ := json.Marshal(d)
		if len(compact) > 2048 {
			t.Fatalf("policy is %d characters", len(compact))
		}
	}
}

func TestNarrowing(t *testing.T) {
	d := policy.Build(policy.Options{Account: "123456789012", Region: "eu-west-1"})
	js := d.JSON()
	for _, want := range []string{
		"arn:aws:s3:::cloudundo-backup-123456789012-eu-west-1/*",
		"arn:aws:dynamodb:eu-west-1:123456789012:table/*/backup/*",
	} {
		if !strings.Contains(js, want) {
			t.Fatalf("missing %s in\n%s", want, js)
		}
	}
	if strings.Contains(js, "KeepS3VersionHistory") {
		t.Fatal("version lock is opt-in")
	}
	if got := (policy.Options{}).BackupBucket(); got != "cloudundo-backup-*" {
		t.Fatalf("BackupBucket = %q", got)
	}
}

func TestValidate(t *testing.T) {
	for _, o := range []policy.Options{{Account: "123"}, {Account: "12345678901a"}, {Region: "Europe"}, {Region: "eu-west-1; rm"}} {
		if o.Validate() == nil {
			t.Fatalf("%+v accepted", o)
		}
	}
	if err := (policy.Options{Account: "123456789012", Region: "ap-southeast-2"}).Validate(); err != nil {
		t.Fatal(err)
	}
}

// guarded runs the fake AWS but refuses what the policy denies, the way
// AWS would, so the whole protect → delete → undo flow is checked under it.
type guarded struct {
	f      *awstest.Fake
	doc    policy.Document
	denied []string
}

func (g *guarded) Run(ctx context.Context, env []string, args ...string) ([]byte, error) {
	for _, ar := range iamOf(args) {
		if g.denies(ar[0], ar[1]) {
			g.denied = append(g.denied, ar[0]+" "+ar[1])
			return nil, &awscli.Error{Args: args, Code: 254, Stderr: "An error occurred (AccessDenied) when calling the operation: explicit deny"}
		}
	}
	return g.f.Run(ctx, env, args...)
}

func (g *guarded) denies(action, resource string) bool {
	for _, s := range g.doc.Statement {
		for _, a := range s.Action {
			if a != action {
				continue
			}
			for _, r := range s.Resource {
				if ok, _ := path.Match(r, resource); ok || globAll(r, resource) {
					return true
				}
			}
		}
	}
	return false
}

// globAll treats "*" as matching across "/" as IAM does.
func globAll(pattern, s string) bool {
	parts := strings.Split(pattern, "*")
	if len(parts) == 1 {
		return pattern == s
	}
	if !strings.HasPrefix(s, parts[0]) {
		return false
	}
	s = s[len(parts[0]):]
	for i, p := range parts[1:] {
		last := i == len(parts)-2
		if last {
			return strings.HasSuffix(s, p)
		}
		j := strings.Index(s, p)
		if j < 0 {
			return false
		}
		s = s[j+len(p):]
	}
	return true
}

func flag(args []string, name string) string {
	for i, a := range args {
		if a == "--"+name && i+1 < len(args) {
			return args[i+1]
		}
	}
	return ""
}

// iamOf maps a CLI call to the IAM actions and resources it needs.
func iamOf(args []string) [][2]string {
	if len(args) < 2 {
		return nil
	}
	b := flag(args, "bucket")
	bucket, object := "arn:aws:s3:::"+b, "arn:aws:s3:::"+b+"/"+flag(args, "key")
	table := "arn:aws:dynamodb:us-east-1:123456789012:table/" + flag(args, "table-name")
	op := args[0] + " " + args[1]
	switch op {
	case "s3api delete-object":
		if flag(args, "version-id") != "" {
			return [][2]string{{"s3:DeleteObjectVersion", object}}
		}
		return [][2]string{{"s3:DeleteObject", object}}
	case "s3api put-bucket-versioning":
		return [][2]string{{"s3:PutBucketVersioning", bucket}}
	case "s3api put-bucket-lifecycle-configuration":
		return [][2]string{{"s3:PutLifecycleConfiguration", bucket}}
	case "s3api delete-bucket":
		return [][2]string{{"s3:DeleteBucket", bucket}}
	case "s3api put-bucket-policy":
		return [][2]string{{"s3:PutBucketPolicy", bucket}}
	case "s3api create-bucket":
		return [][2]string{{"s3:CreateBucket", bucket}}
	case "s3api put-public-access-block":
		return [][2]string{{"s3:PutBucketPublicAccessBlock", bucket}}
	case "s3api copy-object":
		return [][2]string{{"s3:PutObject", object}}
	case "s3 sync":
		dst := strings.TrimPrefix(args[3], "s3://")
		return [][2]string{{"s3:PutObject", "arn:aws:s3:::" + dst + "x"}}
	case "dynamodb create-backup":
		return [][2]string{{"dynamodb:CreateBackup", table}}
	case "dynamodb delete-backup":
		return [][2]string{{"dynamodb:DeleteBackup", flag(args, "backup-arn")}}
	case "dynamodb restore-table-from-backup":
		return [][2]string{{"dynamodb:RestoreTableFromBackup", flag(args, "backup-arn")}}
	case "dynamodb put-item":
		return [][2]string{{"dynamodb:PutItem", table}}
	case "rds delete-db-snapshot":
		return [][2]string{{"rds:DeleteDBSnapshot", "arn:aws:rds:us-east-1:123456789012:snapshot:" + flag(args, "db-snapshot-identifier")}}
	case "rds create-db-snapshot":
		return [][2]string{{"rds:CreateDBSnapshot", "arn:aws:rds:us-east-1:123456789012:snapshot:" + flag(args, "db-snapshot-identifier")}}
	case "rds restore-db-instance-from-db-snapshot":
		return [][2]string{{"rds:RestoreDBInstanceFromDBSnapshot", "arn:aws:rds:us-east-1:123456789012:snapshot:" + flag(args, "db-snapshot-identifier")}}
	}
	return nil
}

type world struct {
	t   *testing.T
	f   *awstest.Fake
	g   *guarded
	cf  func(awscli.Conn) awscli.Client
	dir string
}

func newWorld(t *testing.T) *world {
	f := awstest.New()
	g := &guarded{f: f, doc: policy.Build(policy.Options{KeepVersions: true})}
	return &world{t: t, f: f, g: g, dir: t.TempDir(),
		cf: func(c awscli.Conn) awscli.Client { return awscli.Client{R: g, C: c} }}
}

func (w *world) protect(line string) *journal.Entry {
	w.t.Helper()
	cfg := protect.Defaults()
	cfg.Now = w.f.Now
	cfg.DataDir = w.dir
	cfg.Sleep = func(time.Duration) {}
	r := protect.Run(context.Background(), effects.Analyze(line), w.cf, cfg)
	if !r.Allowed {
		w.t.Fatalf("%q blocked under the policy: %v (denied: %v)", line, r.Blocked, w.g.denied)
	}
	return &journal.Entry{ID: 1, Command: line, Undo: r.Undo, Status: "protected"}
}

func (w *world) undo(e *journal.Entry) {
	w.t.Helper()
	for i := 0; i < 3; i++ { // a table restore takes two runs
		p, err := undo.For(context.Background(), e, w.cf, w.dir)
		if err != nil {
			w.t.Fatalf("undo plan: %v", err)
		}
		if _, err := p.Apply(context.Background()); err != nil {
			w.t.Fatalf("undo under the policy: %v (denied: %v)", err, w.g.denied)
		}
		if !p.Pending {
			return
		}
	}
}

func TestCloudundoStillWorksUnderThePolicy(t *testing.T) {
	t.Run("object delete", func(t *testing.T) {
		w := newWorld(t)
		w.f.CreateBucket("b", "us-east-1", "")
		w.f.Put("b", "k", "v1")
		e := w.protect("aws s3 rm s3://b/k")
		w.f.Delete("b", "k")
		w.undo(e)
		if got, ok := w.f.Get("b", "k"); !ok || got != "v1" {
			t.Fatalf("k = %q %v", got, ok)
		}
	})
	t.Run("bucket delete", func(t *testing.T) {
		w := newWorld(t)
		w.f.CreateBucket("b", "us-east-1", "")
		w.f.Put("b", "k", "v1")
		e := w.protect("aws s3 rb s3://b --force")
		w.f.RemoveBucket("b")
		w.undo(e)
		if got, ok := w.f.Get("b", "k"); !ok || got != "v1" {
			t.Fatalf("k = %q %v", got, ok)
		}
	})
	t.Run("table delete", func(t *testing.T) {
		w := newWorld(t)
		w.f.CreateTable("T", "pk")
		w.f.PutItem("T", `{"pk":{"S":"a"}}`)
		e := w.protect("aws dynamodb delete-table --table-name T")
		w.f.DropTable("T")
		w.undo(e)
		if w.f.Item("T", `{"pk":{"S":"a"}}`) == "" {
			t.Fatal("item not back")
		}
	})
	t.Run("database delete", func(t *testing.T) {
		w := newWorld(t)
		w.f.CreateDB(awscli.DBInfo{ID: "db1", Engine: "postgres", Class: "db.t4g.micro"}, "rows")
		e := w.protect("aws rds delete-db-instance --db-instance-identifier db1 --skip-final-snapshot")
		w.f.DropDB("db1", false, "")
		w.undo(e)
		if _, data, ok := w.f.DB("db1", false); !ok || data != "rows" {
			t.Fatal("database not back")
		}
	})
	t.Run("item delete", func(t *testing.T) {
		w := newWorld(t)
		w.f.CreateTable("T", "pk")
		w.f.PutItem("T", `{"pk":{"S":"a"},"n":{"N":"1"}}`)
		e := w.protect(`aws dynamodb delete-item --table-name T --key '{"pk":{"S":"a"}}'`)
		w.f.DeleteItem("T", `{"pk":{"S":"a"}}`)
		w.undo(e)
		if w.f.Item("T", `{"pk":{"S":"a"}}`) == "" {
			t.Fatal("item not back")
		}
	})
}

func TestThePolicyStopsWhatTheHookCannotSee(t *testing.T) {
	w := newWorld(t)
	backup := "cloudundo-backup-123456789012-us-east-1"
	tableBackup := "arn:aws:dynamodb:us-east-1:123456789012:table/T/backup/1"
	for _, args := range [][]string{
		{"s3api", "delete-object", "--bucket", backup, "--key", "b/x"},
		{"s3api", "delete-bucket", "--bucket", backup},
		{"s3api", "put-bucket-lifecycle-configuration", "--bucket", backup},
		{"s3api", "put-bucket-versioning", "--bucket", backup},
		{"s3api", "put-bucket-policy", "--bucket", backup},
		{"s3api", "delete-object", "--bucket", "user-data", "--key", "k", "--version-id", "v1"},
		{"dynamodb", "delete-backup", "--backup-arn", tableBackup},
		{"rds", "delete-db-snapshot", "--db-snapshot-identifier", "cloudundo-prod-db-20261019-090000"},
	} {
		if _, err := w.g.Run(context.Background(), nil, args...); !awscli.Has(err, "AccessDenied") {
			t.Errorf("%v allowed", args)
		}
	}
	// Ordinary work on the user's own buckets stays allowed.
	for _, args := range [][]string{
		{"s3api", "delete-object", "--bucket", "user-data", "--key", "k"},
		{"s3api", "put-bucket-versioning", "--bucket", "user-data"},
		{"s3api", "put-bucket-lifecycle-configuration", "--bucket", "user-data"},
		{"s3api", "create-bucket", "--bucket", backup},
	} {
		for _, ar := range iamOf(args) {
			if w.g.denies(ar[0], ar[1]) {
				t.Errorf("%v denied", args)
			}
		}
	}
}
