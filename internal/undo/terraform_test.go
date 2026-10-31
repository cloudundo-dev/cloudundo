package undo

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/cloudundo-dev/cloudundo/internal/awscli"
	"github.com/cloudundo-dev/cloudundo/internal/effects"
	"github.com/cloudundo-dev/cloudundo/internal/journal"
	"github.com/cloudundo-dev/cloudundo/internal/protect"
)

// grigorevPlan is the shape of `terraform show -json` for a destroy like
// the February 2026 incident: database, bucket, table and network.
const grigorevPlan = `{
  "format_version": "1.2",
  "resource_changes": [
    {"address": "aws_db_instance.main", "mode": "managed", "type": "aws_db_instance",
     "change": {"actions": ["delete"], "before": {"identifier": "courses-db", "skip_final_snapshot": true,
       "arn": "arn:aws:rds:eu-west-1:123456789012:db:courses-db"}}},
    {"address": "aws_s3_bucket.uploads", "mode": "managed", "type": "aws_s3_bucket",
     "change": {"actions": ["delete"], "before": {"bucket": "course-uploads", "force_destroy": true,
       "arn": "arn:aws:s3:::course-uploads", "region": "eu-west-1"}}},
    {"address": "aws_dynamodb_table.sessions", "mode": "managed", "type": "aws_dynamodb_table",
     "change": {"actions": ["delete"], "before": {"name": "sessions",
       "arn": "arn:aws:dynamodb:eu-west-1:123456789012:table/sessions"}}},
    {"address": "aws_vpc.main", "mode": "managed", "type": "aws_vpc",
     "change": {"actions": ["delete"], "before": {"id": "vpc-1"}}},
    {"address": "aws_ecs_cluster.app", "mode": "managed", "type": "aws_ecs_cluster",
     "change": {"actions": ["delete"], "before": {"name": "app"}}},
    {"address": "aws_iam_role.app", "mode": "managed", "type": "aws_iam_role",
     "change": {"actions": ["no-op"], "before": {"name": "app"}}},
    {"address": "data.aws_caller_identity.me", "mode": "data", "type": "aws_caller_identity",
     "change": {"actions": ["read"], "before": null}}
  ]
}`

func cannedPlanner(body string, err error) protect.Planner {
	return func(context.Context, effects.Terraform, awscli.Conn) ([]byte, error) {
		return []byte(body), err
	}
}

func (w *world) protectTF(line string, planner protect.Planner) (*journal.Entry, protect.Result) {
	w.t.Helper()
	cfg := protect.Defaults()
	cfg.Now = w.f.Now
	cfg.Sleep = func(time.Duration) {}
	cfg.Planner = planner
	r := protect.Run(context.Background(), effects.Analyze(line), w.cf, cfg)
	return &journal.Entry{ID: 3, Command: line, Undo: r.Undo, Status: "protected"}, r
}

func TestUndoTerraformDestroy(t *testing.T) {
	w := newWorld(t)
	w.f.CreateDB(awscli.DBInfo{ID: "courses-db", Engine: "postgres", Class: "db.t4g.medium", BackupRetention: 7}, "1,943,200 answers")
	w.f.CreateBucket("course-uploads", "eu-west-1", "")
	w.f.Put("course-uploads", "videos/intro.mp4", "video")
	w.f.CreateTable("sessions", "pk")
	w.f.PutItem("sessions", `{"pk":{"S":"s1"}}`)

	e, r := w.protectTF("cd infra && terraform destroy -auto-approve", cannedPlanner(grigorevPlan, nil))
	if !r.Allowed {
		t.Fatalf("blocked: %v", r.Blocked)
	}
	if len(r.Undo) != 3 {
		t.Fatalf("undo steps = %d: %+v", len(r.Undo), r.Undo)
	}
	notes := strings.Join(r.Notes, " ")
	if !strings.Contains(notes, "2 resource(s) that cloudundo does not restore") || !strings.Contains(notes, "aws_vpc.main") {
		t.Fatalf("notes = %s", notes)
	}
	for _, s := range r.Undo {
		if s.Kind == "rds-snapshot" && s.Conn.Region != "eu-west-1" {
			t.Fatalf("region from the ARN not used: %+v", s.Conn)
		}
	}

	// terraform destroy runs.
	w.f.DropDB("courses-db", false, "")
	w.f.RemoveBucket("course-uploads")
	w.f.DropTable("sessions")

	for i := 0; i < 3; i++ {
		p := w.plan(e, "")
		w.apply(p)
		if !p.Pending {
			break
		}
	}
	if _, data, ok := w.f.DB("courses-db", false); !ok || data != "1,943,200 answers" {
		t.Fatalf("database = %v %q", ok, data)
	}
	if got, ok := w.f.Get("course-uploads", "videos/intro.mp4"); !ok || got != "video" {
		t.Fatalf("bucket = %v %q", ok, got)
	}
	if w.f.Item("sessions", `{"pk":{"S":"s1"}}`) == "" {
		t.Fatal("table not back")
	}
}

func TestTerraformBlocks(t *testing.T) {
	for name, tc := range map[string]struct {
		line    string
		planner protect.Planner
		why     string
	}{
		"plan fails": {"terraform destroy", cannedPlanner("", errors.New("terraform plan failed: no state")), "could not see what"},
		"bad json":   {"terraform destroy", cannedPlanner("not json", nil), "could not read the terraform plan"},
		"other account": {"terraform destroy", cannedPlanner(`{"resource_changes":[{"address":"aws_dynamodb_table.t","mode":"managed","type":"aws_dynamodb_table",
			"change":{"actions":["delete"],"before":{"name":"t","arn":"arn:aws:dynamodb:us-east-1:999999999999:table/t"}}}]}`, nil), "account 999999999999"},
		"unknown dir": {"cd $D && terraform destroy", cannedPlanner(grigorevPlan, nil), "cannot tell exactly"},
		"versioning removed alone": {"terraform apply", cannedPlanner(`{"resource_changes":[{"address":"aws_s3_bucket_versioning.v","mode":"managed","type":"aws_s3_bucket_versioning",
			"change":{"actions":["delete"],"before":{"bucket":"keep-me"}}}]}`, nil), "suspends versioning"},
		"snapshot deleted": {"terraform destroy", cannedPlanner(`{"resource_changes":[{"address":"aws_db_snapshot.s","mode":"managed","type":"aws_db_snapshot",
			"change":{"actions":["delete"],"before":{"db_snapshot_identifier":"keep"}}}]}`, nil), "deletes a database snapshot"},
		"backup bucket": {"terraform destroy", cannedPlanner(`{"resource_changes":[{"address":"aws_s3_bucket.b","mode":"managed","type":"aws_s3_bucket",
			"change":{"actions":["delete"],"before":{"bucket":"cloudundo-backup-123456789012-us-east-1"}}}]}`, nil), "keeps its backups"},
		// Found in Cursor: the plan was read where there was no state, saw
		// nothing to delete, and the destroy then deleted three resources.
		"empty state": {"terraform destroy -auto-approve", cannedPlanner(`{"format_version":"1.2","resource_changes":[]}`, nil), "found no resources in its state"},
		"missing name": {"terraform destroy", cannedPlanner(`{"resource_changes":[{"address":"aws_dynamodb_table.t","mode":"managed","type":"aws_dynamodb_table",
			"change":{"actions":["delete"],"before":{}}}]}`, nil), "cannot tell exactly"},
	} {
		t.Run(name, func(t *testing.T) {
			w := newWorld(t)
			_, r := w.protectTF(tc.line, tc.planner)
			if r.Allowed || !strings.Contains(strings.Join(r.Blocked, " "), tc.why) {
				t.Fatalf("r = %+v", r)
			}
		})
	}
}

func TestTerraformReplaceAndNoData(t *testing.T) {
	w := newWorld(t)
	w.f.CreateTable("t", "pk")
	plan := `{"resource_changes":[
	  {"address":"aws_dynamodb_table.t","mode":"managed","type":"aws_dynamodb_table",
	   "change":{"actions":["delete","create"],"before":{"name":"t","arn":"arn:aws:dynamodb:us-east-1:123456789012:table/t"}}},
	  {"address":"aws_s3_bucket_versioning.v","mode":"managed","type":"aws_s3_bucket_versioning",
	   "change":{"actions":["update"],"before":{"bucket":"b"}}}]}`
	_, r := w.protectTF("terraform apply -auto-approve", cannedPlanner(plan, nil))
	if !r.Allowed || len(r.Undo) != 1 || r.Undo[0].Kind != "ddb-table-backup" {
		t.Fatalf("a replaced table must be backed up: %+v", r)
	}

	_, r = w.protectTF("terraform apply", cannedPlanner(`{"resource_changes":[{"address":"aws_vpc.v","mode":"managed","type":"aws_vpc","change":{"actions":["delete"],"before":{}}}]}`, nil))
	if !r.Allowed || len(r.Undo) != 0 || !strings.Contains(strings.Join(r.Notes, " "), "deletes no data") {
		t.Fatalf("r = %+v", r)
	}
}

func TestTerraformDestroyOfStateWithNothingToDelete(t *testing.T) {
	w := newWorld(t)
	plan := `{"resource_changes":[],"prior_state":{"values":{"root_module":{"child_modules":[{"resources":[{"mode":"data"},{"mode":"managed"}]}]}}}}`
	_, r := w.protectTF("terraform destroy", cannedPlanner(plan, nil))
	if !r.Allowed || !strings.Contains(strings.Join(r.Notes, " "), "Plan read in") {
		t.Fatalf("r = %+v", r)
	}
}

func TestTerraformVersioningGoesWithItsBucket(t *testing.T) {
	w := newWorld(t)
	w.f.CreateBucket("site", "us-east-1", "Enabled")
	w.f.Put("site", "index.html", "hi")
	plan := `{"resource_changes":[
	  {"address":"aws_s3_bucket.site","mode":"managed","type":"aws_s3_bucket","change":{"actions":["delete"],"before":{"bucket":"site","force_destroy":true}}},
	  {"address":"aws_s3_bucket_versioning.site","mode":"managed","type":"aws_s3_bucket_versioning","change":{"actions":["delete"],"before":{"bucket":"site"}}}]}`
	_, r := w.protectTF("terraform destroy", cannedPlanner(plan, nil))
	if !r.Allowed || len(r.Undo) != 1 || r.Undo[0].Kind != "s3-bucket-copy" {
		t.Fatalf("r = %+v", r)
	}
}

func TestTerraformBlocksDataItCannotBackUp(t *testing.T) {
	w := newWorld(t)
	plan := `{"resource_changes":[{"address":"aws_ebs_volume.data","mode":"managed","type":"aws_ebs_volume",
	  "change":{"actions":["delete"],"before":{"id":"vol-1","arn":"arn:aws:ec2:us-east-1:123456789012:volume/vol-1"}}}]}`
	_, r := w.protectTF("terraform destroy", cannedPlanner(plan, nil))
	if r.Allowed || !strings.Contains(strings.Join(r.Blocked, " "), "aws_ebs_volume.data, which holds data cloudundo cannot back up yet") {
		t.Fatalf("r = %+v", r)
	}
}

// A destroy that deletes a bucket and an object in it (aws_s3_object).
// Found on real AWS (Test 7): the object was protected twice, by
// versioning and by the bucket copy, and undo stopped at the missing
// bucket before reaching the copy.
func TestUndoTerraformDestroyOfBucketAndItsObject(t *testing.T) {
	w := newWorld(t)
	w.f.CreateBucket("files", "eu-west-1", "")
	w.f.Put("files", "hello.txt", "hello")
	plan := `{"format_version": "1.2", "resource_changes": [
	  {"address": "aws_s3_object.hello", "mode": "managed", "type": "aws_s3_object",
	   "change": {"actions": ["delete"], "before": {"bucket": "files", "key": "hello.txt",
	     "arn": "arn:aws:s3:::files/hello.txt", "region": "eu-west-1"}}},
	  {"address": "aws_s3_bucket.files", "mode": "managed", "type": "aws_s3_bucket",
	   "change": {"actions": ["delete"], "before": {"bucket": "files", "force_destroy": true,
	     "arn": "arn:aws:s3:::files", "region": "eu-west-1"}}}
	]}`
	e, r := w.protectTF("terraform destroy -auto-approve", cannedPlanner(plan, nil))
	if !r.Allowed {
		t.Fatalf("blocked: %v", r.Blocked)
	}
	// The bucket copy covers the object too; versioning would only make
	// emptying and deleting the bucket fail.
	if len(r.Undo) != 1 || r.Undo[0].Kind != "s3-bucket-copy" {
		t.Fatalf("want only the bucket copy, got %+v", r.Undo)
	}
	if w.f.Versioning("files") != "" {
		t.Fatal("versioning was turned on for a bucket the command deletes")
	}
	// Entries recorded by earlier versions protected such objects twice;
	// undo must still work for them.
	e.Undo = append([]journal.UndoStep{{Kind: "s3-versions", Bucket: "files", Key: "hello.txt",
		Since: r.Undo[0].Since, Conn: r.Undo[0].Conn}}, e.Undo...)

	w.f.RemoveBucket("files") // terraform destroy: objects, versions and bucket

	p, err := For(context.Background(), e, w.cf, t.TempDir())
	if err != nil {
		t.Fatalf("undo plan stopped: %v", err)
	}
	if _, err := p.Apply(context.Background()); err != nil {
		t.Fatalf("apply: %v", err)
	}
	if v, ok := w.f.Get("files", "hello.txt"); !ok || v != "hello" {
		t.Fatalf("hello.txt = %q, %v", v, ok)
	}

	// The second undo run (to finish slow restores) must not call the
	// restored object new: its history went with the deleted bucket.
	p, err = For(context.Background(), e, w.cf, t.TempDir())
	if err != nil {
		t.Fatalf("second undo plan: %v", err)
	}
	if strings.Contains(p.Summary(), "did not exist before") || len(p.Actions) != 0 {
		t.Fatalf("second run:\n%s", p.Summary())
	}
}
