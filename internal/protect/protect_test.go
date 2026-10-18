package protect

import (
	"context"
	"strings"
	"testing"

	"github.com/cloudundo/cloudundo/internal/awscli"
	"github.com/cloudundo/cloudundo/internal/awstest"
	"github.com/cloudundo/cloudundo/internal/effects"
)

func setup(t *testing.T) (*awstest.Fake, ClientFor, Config) {
	t.Helper()
	f := awstest.New()
	cfg := Defaults()
	cfg.Now = f.Now
	return f, func(c awscli.Conn) awscli.Client { return awscli.Client{R: f, C: c} }, cfg
}

func protect(t *testing.T, f *awstest.Fake, cf ClientFor, cfg Config, line string) Result {
	t.Helper()
	return Run(context.Background(), effects.Analyze(line), cf, cfg)
}

func calls(f *awstest.Fake, op string) int {
	n := 0
	for _, c := range f.Calls {
		if strings.Contains(strings.Join(c, " "), op) {
			n++
		}
	}
	return n
}

func TestUnversionedBucketGetsVersioningAndExpiry(t *testing.T) {
	f, cf, cfg := setup(t)
	f.CreateBucket("uploads", "us-east-1", "")
	r := protect(t, f, cf, cfg, "aws s3 rm s3://uploads/a.jpg")
	if !r.Allowed || len(r.Blocked) > 0 {
		t.Fatalf("blocked: %v", r.Blocked)
	}
	if f.Versioning("uploads") != "Enabled" {
		t.Fatal("versioning not enabled")
	}
	if lc := f.Lifecycle("uploads"); !strings.Contains(lc, ExpiryRuleID) || !strings.Contains(lc, `"NoncurrentDays":7`) {
		t.Fatalf("lifecycle = %s", lc)
	}
	if len(r.Undo) != 1 || r.Undo[0].Kind != "s3-versions" || r.Undo[0].Key != "a.jpg" {
		t.Fatalf("undo = %+v", r.Undo)
	}
}

func TestExistingLifecycleRulesAreKept(t *testing.T) {
	f, cf, cfg := setup(t)
	f.CreateBucket("logs", "us-east-1", "")
	existing := `{"ID":"archive","Status":"Enabled","Filter":{"Prefix":"old/"},"Transitions":[{"Days":30,"StorageClass":"GLACIER"}],"SomeFutureField":{"x":1}}`
	f.SetLifecycle("logs", `{"Rules":[`+existing+`],"TransitionDefaultMinimumObjectSize":"all_storage_classes_128K"}`)
	if r := protect(t, f, cf, cfg, "aws s3 rm s3://logs/x"); !r.Allowed {
		t.Fatalf("blocked: %v", r.Blocked)
	}
	lc := f.Lifecycle("logs")
	if !strings.Contains(lc, existing) {
		t.Fatalf("existing rule changed or lost:\n%s", lc)
	}
	if !strings.Contains(lc, ExpiryRuleID) {
		t.Fatalf("our rule missing:\n%s", lc)
	}
	if calls(f, "--transition-default-minimum-object-size all_storage_classes_128K") != 1 {
		t.Fatal("bucket-level lifecycle setting was not passed back")
	}
}

func TestAlreadyVersionedBucketIsLeftAlone(t *testing.T) {
	f, cf, cfg := setup(t)
	f.CreateBucket("kept", "us-east-1", "Enabled")
	r := protect(t, f, cf, cfg, "aws s3 rm s3://kept/k")
	if !r.Allowed {
		t.Fatalf("blocked: %v", r.Blocked)
	}
	// An expiry rule here would delete old versions the owner chose to keep.
	if f.Lifecycle("kept") != "" || calls(f, "put-bucket-lifecycle") != 0 {
		t.Fatal("lifecycle was touched on an already versioned bucket")
	}
}

func TestSuspendedBucketIsReenabledWithoutExpiry(t *testing.T) {
	f, cf, cfg := setup(t)
	f.CreateBucket("paused", "us-east-1", "Suspended")
	r := protect(t, f, cf, cfg, "aws s3 rm s3://paused/k")
	if !r.Allowed || f.Versioning("paused") != "Enabled" {
		t.Fatalf("allowed=%v versioning=%s", r.Allowed, f.Versioning("paused"))
	}
	if calls(f, "put-bucket-lifecycle") != 0 {
		t.Fatal("expiry rule added to a bucket with older versions")
	}
}

func TestExistingNoncurrentRuleIsRespected(t *testing.T) {
	f, cf, cfg := setup(t)
	f.CreateBucket("b", "us-east-1", "")
	f.SetLifecycle("b", `{"Rules":[{"ID":"theirs","Status":"Enabled","Filter":{},"NoncurrentVersionExpiration":{"NoncurrentDays":30}}]}`)
	protect(t, f, cf, cfg, "aws s3 rm s3://b/k")
	if strings.Contains(f.Lifecycle("b"), ExpiryRuleID) {
		t.Fatal("added a second expiry rule")
	}
}

func TestBlocksBeforeChangingAnything(t *testing.T) {
	for _, line := range []string{
		"aws s3 rm s3://$BUCKET --recursive",
		"cat keys | xargs aws s3 rm",
		"aws s3api delete-objects --bucket b --delete file://keys.json",
		"aws s3api put-bucket-versioning --bucket b --versioning-configuration Status=Suspended",
		"aws s3 rb s3://cloudundo-backup-123456789012-us-east-1 --force",
		"aws s3 rm s3://ok/k && aws s3 rm s3://$OTHER/k",
	} {
		f, cf, cfg := setup(t)
		f.CreateBucket("ok", "us-east-1", "")
		r := protect(t, f, cf, cfg, line)
		if r.Allowed || len(r.Blocked) == 0 {
			t.Errorf("%q was allowed", line)
		}
		if len(f.Calls) != 0 {
			t.Errorf("%q: AWS was called before blocking: %v", line, f.Calls)
		}
	}
}

func TestAWSErrorsBlock(t *testing.T) {
	f, cf, cfg := setup(t)
	f.CreateBucket("b", "us-east-1", "")
	f.Fail["get-bucket-versioning"] = "An error occurred (AccessDenied)"
	r := protect(t, f, cf, cfg, "aws s3 rm s3://b/k")
	if r.Allowed || !strings.Contains(r.Blocked[0], "AccessDenied") {
		t.Fatalf("got %+v", r)
	}

	f, cf, cfg = setup(t)
	f.CreateBucket("b", "us-east-1", "")
	f.Fail["put-bucket-versioning"] = "An error occurred (AccessDenied)"
	if r := protect(t, f, cf, cfg, "aws s3 rm s3://b/k"); r.Allowed {
		t.Fatal("allowed although versioning could not be turned on")
	}
}

func TestExpiryFailureStillProtects(t *testing.T) {
	f, cf, cfg := setup(t)
	f.CreateBucket("b", "us-east-1", "")
	f.Fail["put-bucket-lifecycle"] = "An error occurred (AccessDenied)"
	r := protect(t, f, cf, cfg, "aws s3 rm s3://b/k")
	if !r.Allowed || f.Versioning("b") != "Enabled" {
		t.Fatal("versioning protects the data even without the expiry rule")
	}
	if !strings.Contains(strings.Join(r.Notes, " "), "could not add") {
		t.Fatalf("notes = %v", r.Notes)
	}
}

func TestMissingBucketIsAllowed(t *testing.T) {
	f, cf, cfg := setup(t)
	if r := protect(t, f, cf, cfg, "aws s3 rm s3://nope/k"); !r.Allowed {
		t.Fatalf("blocked: %v", r.Blocked)
	}
	if r := protect(t, f, cf, cfg, "aws s3 rb s3://nope --force"); !r.Allowed {
		t.Fatalf("blocked: %v", r.Blocked)
	}
}

func TestOneVersioningCheckPerBucket(t *testing.T) {
	f, cf, cfg := setup(t)
	f.CreateBucket("b", "us-east-1", "")
	protect(t, f, cf, cfg, "aws s3 rm s3://b/x && aws s3 rm s3://b/y && aws s3 cp f s3://b/z")
	if n := calls(f, "get-bucket-versioning"); n != 1 {
		t.Fatalf("versioning checked %d times", n)
	}
}

func TestProfileAndRegionAreUsed(t *testing.T) {
	f, cf, cfg := setup(t)
	f.CreateBucket("b", "eu-west-1", "")
	protect(t, f, cf, cfg, "AWS_PROFILE=prod aws --region eu-west-1 s3 rm s3://b/k")
	for i, c := range f.Calls {
		joined := strings.Join(c, " ")
		if !strings.Contains(joined, "--region eu-west-1") {
			t.Errorf("call without the region: %s", joined)
		}
		if !strings.Contains(strings.Join(f.Env[i], " "), "AWS_PROFILE=prod") {
			t.Errorf("call without the profile: %s", joined)
		}
	}
}

func TestBucketDeleteIsCopiedAndVerified(t *testing.T) {
	f, cf, cfg := setup(t)
	f.CreateBucket("user-data", "eu-west-1", "")
	f.Put("user-data", "a.txt", "alpha")
	f.Put("user-data", "dir/b.txt", "bravo")

	r := protect(t, f, cf, cfg, "aws s3 rb s3://user-data --force")
	if !r.Allowed {
		t.Fatalf("blocked: %v", r.Blocked)
	}
	backup := "cloudundo-backup-123456789012-eu-west-1"
	if !f.HasBucket(backup) {
		t.Fatal("no backup bucket")
	}
	if len(r.Undo) != 1 || r.Undo[0].Kind != "s3-bucket-copy" {
		t.Fatalf("undo = %+v", r.Undo)
	}
	got, ok := f.Get(backup, r.Undo[0].BackupPrefix+"dir/b.txt")
	if !ok || got != "bravo" {
		t.Fatalf("backup content = %q, %v", got, ok)
	}
	if calls(f, "put-public-access-block") != 1 {
		t.Fatal("backup bucket was not made private")
	}
}

func TestBucketDeleteLimits(t *testing.T) {
	f, cf, cfg := setup(t)
	cfg.MaxCopyBytes = 4
	f.CreateBucket("big", "us-east-1", "")
	f.Put("big", "k", "more than four bytes")
	r := protect(t, f, cf, cfg, "aws s3 rb s3://big --force")
	if r.Allowed || !strings.Contains(r.Blocked[0], "more than") {
		t.Fatalf("got %+v", r)
	}

	// Without --force AWS refuses to delete a bucket that has objects.
	f, cf, cfg = setup(t)
	f.CreateBucket("full", "us-east-1", "")
	f.Put("full", "k", "v")
	r = protect(t, f, cf, cfg, "aws s3 rb s3://full")
	if !r.Allowed || calls(f, "sync") != 0 {
		t.Fatalf("plain rb: allowed=%v syncs=%d", r.Allowed, calls(f, "sync"))
	}
}

func TestFailedCopyBlocksTheDelete(t *testing.T) {
	f, cf, cfg := setup(t)
	f.CreateBucket("b", "us-east-1", "")
	f.Put("b", "k", "v")
	f.Fail["s3 sync"] = "upload failed: connection reset"
	r := protect(t, f, cf, cfg, "aws s3 rb s3://b --force")
	if r.Allowed || len(r.Undo) != 0 {
		t.Fatalf("allowed after a failed copy: %+v", r)
	}

	f, cf, cfg = setup(t)
	f.CreateBucket("b", "us-east-1", "")
	f.Put("b", "k", "v")
	f.Fail["list-objects-v2 --bucket cloudundo-backup"] = "An error occurred (InternalError)"
	if r := protect(t, f, cf, cfg, "aws s3 rb s3://b --force"); r.Allowed {
		t.Fatal("allowed without verifying the copy")
	}
}

func TestHuman(t *testing.T) {
	for n, want := range map[int64]string{0: "0 B", 1023: "1023 B", 1024: "1.0 KB", 5 << 30: "5.0 GB"} {
		if got := human(n); got != want {
			t.Errorf("human(%d) = %q, want %q", n, got, want)
		}
	}
}

func TestBackupBucketIsVersioned(t *testing.T) {
	f, cf, cfg := setup(t)
	f.CreateBucket("user-data", "eu-west-1", "")
	f.Put("user-data", "a.txt", "alpha")
	r := protect(t, f, cf, cfg, "aws s3 rb s3://user-data --force")
	if !r.Allowed {
		t.Fatalf("blocked: %v", r.Blocked)
	}
	if v := f.Versioning("cloudundo-backup-123456789012-eu-west-1"); v != "Enabled" {
		t.Fatalf("backup bucket versioning = %q", v)
	}
}

func TestBackupStillMadeWhenItsVersioningIsDenied(t *testing.T) {
	f, cf, cfg := setup(t)
	f.CreateBucket("user-data", "eu-west-1", "")
	f.Put("user-data", "a.txt", "alpha")
	f.Fail["put-bucket-versioning --bucket cloudundo-backup-"] = "An error occurred (AccessDenied)"
	r := protect(t, f, cf, cfg, "aws s3 rb s3://user-data --force")
	if !r.Allowed || len(r.Undo) != 1 {
		t.Fatalf("blocked: %v", r.Blocked)
	}
	if !strings.Contains(strings.Join(r.Notes, "\n"), "Could not turn on versioning for the backup bucket") {
		t.Fatalf("notes = %v", r.Notes)
	}
}
