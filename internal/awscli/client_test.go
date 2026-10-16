package awscli

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// fakeBin writes a shell script standing in for aws.
func fakeBin(t *testing.T, script string) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), "aws")
	if err := os.WriteFile(p, []byte("#!/bin/sh\n"+script), 0o755); err != nil {
		t.Fatal(err)
	}
	return p
}

func TestExecPassesArgsAndEnv(t *testing.T) {
	log := filepath.Join(t.TempDir(), "log")
	bin := fakeBin(t, `echo "$@" > `+log+`; echo "profile=$AWS_PROFILE key=$AWS_SECRET_ACCESS_KEY pager=[$AWS_PAGER]" >> `+log+`; echo '{"Status":"Enabled"}'`)
	c := Client{R: Exec{Bin: bin}, C: ConnFromEnv(map[string]string{"AWS_PROFILE": "prod", "AWS_SECRET_ACCESS_KEY": "s"})}
	c.C.Region = "eu-west-1"

	st, err := c.VersioningStatus(context.Background(), "b")
	if err != nil || st != "Enabled" {
		t.Fatalf("status %q, err %v", st, err)
	}
	got, _ := os.ReadFile(log)
	want := "s3api get-bucket-versioning --bucket b --region eu-west-1 --output json\nprofile=prod key=s pager=[]\n"
	if string(got) != want {
		t.Fatalf("got\n%s\nwant\n%s", got, want)
	}
}

func TestExecErrors(t *testing.T) {
	bin := fakeBin(t, `echo "An error occurred (NoSuchLifecycleConfiguration) when calling" >&2; exit 254`)
	c := Client{R: Exec{Bin: bin}}
	_, ok, err := c.GetLifecycle(context.Background(), "b")
	if err != nil || ok {
		t.Fatalf("missing lifecycle should be ok=false, err=nil; got %v, %v", ok, err)
	}

	bin = fakeBin(t, `echo "An error occurred (AccessDenied)" >&2; exit 254`)
	c = Client{R: Exec{Bin: bin}}
	_, err = c.VersioningStatus(context.Background(), "b")
	if err == nil || !Has(err, "AccessDenied") || !strings.Contains(err.Error(), "get-bucket-versioning") {
		t.Fatalf("err = %v", err)
	}

	c = Client{R: Exec{Bin: filepath.Join(t.TempDir(), "missing")}}
	if _, err := c.VersioningStatus(context.Background(), "b"); err == nil {
		t.Fatal("missing aws binary should fail")
	}
}

func TestSecretsNotSerialized(t *testing.T) {
	c := ConnFromEnv(map[string]string{"AWS_SECRET_ACCESS_KEY": "s", "AWS_PROFILE": "p"})
	b, _ := jsonMarshal(c)
	if strings.Contains(string(b), "AWS_SECRET") {
		t.Fatalf("secret leaked: %s", b)
	}
}
