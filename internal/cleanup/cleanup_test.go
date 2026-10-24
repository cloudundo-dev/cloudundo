package cleanup

import (
	"context"
	"strings"
	"testing"

	"github.com/cloudundo/cloudundo/internal/awscli"
	"github.com/cloudundo/cloudundo/internal/awstest"
)

func TestParseAge(t *testing.T) {
	for in, ok := range map[string]bool{"30d": true, "0d": true, "12h": true, "90m": true,
		"x": false, "-1d": false, "3.5d": false, "-2h": false, "d": false, "": false} {
		if _, err := ParseAge(in); (err == nil) != ok {
			t.Errorf("%q: err %v", in, err)
		}
	}
}

// Cleanup only ever deletes under one of its own copy prefixes.
func TestDeleteCopyRefusesOtherPrefixes(t *testing.T) {
	f := awstest.New()
	f.CreateBucket("cloudundo-backup-1-us-east-1", "us-east-1", "Enabled")
	f.Put("cloudundo-backup-1-us-east-1", "files/20261022T162314Z/a", "A")
	f.Put("cloudundo-backup-1-us-east-1", "other/b", "B")
	c := awscli.Client{R: f}
	for _, prefix := range []string{"", "files", "files/", "/"} {
		if _, err := deleteCopy(context.Background(), c, "cloudundo-backup-1-us-east-1", prefix); err == nil || !strings.Contains(err.Error(), "not a cloudundo copy") {
			t.Errorf("prefix %q: err %v", prefix, err)
		}
	}
	if gone, err := deleteCopy(context.Background(), c, "cloudundo-backup-1-us-east-1", "files/20261022T162314Z/"); err != nil || gone {
		t.Fatalf("gone %v err %v", gone, err)
	}
	if gone, err := deleteCopy(context.Background(), c, "cloudundo-backup-1-us-east-1", "files/20261022T162314Z/"); err != nil || !gone {
		t.Fatalf("a second time it is already gone: gone %v err %v", gone, err)
	}
	if f.Count("cloudundo-backup-1-us-east-1", "files/") != 0 || f.Count("cloudundo-backup-1-us-east-1", "other/") != 1 {
		t.Fatal("deleted the wrong objects")
	}
}
