package journal

import (
	"testing"

	"github.com/cloudundo/cloudundo/internal/effects"
)

func TestAppendListGet(t *testing.T) {
	j, err := Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}

	got, err := j.List()
	if err != nil || len(got) != 0 {
		t.Fatalf("empty journal: got %v, %v", got, err)
	}

	e1, err := j.Append(Entry{Command: "aws s3 rm s3://b/k", Status: "seen",
		Effects: []effects.Effect{{Kind: effects.DeleteObjects, Bucket: "b", Key: "k"}}})
	if err != nil {
		t.Fatal(err)
	}
	e2, err := j.Append(Entry{Command: "aws s3 rb s3://b", Status: "seen"})
	if err != nil {
		t.Fatal(err)
	}
	if e1.ID != 1 || e2.ID != 2 {
		t.Fatalf("ids = %d, %d; want 1, 2", e1.ID, e2.ID)
	}

	all, err := j.List()
	if err != nil {
		t.Fatal(err)
	}
	if len(all) != 2 || all[0].Effects[0].Bucket != "b" {
		t.Fatalf("list = %+v", all)
	}

	e, ok, err := j.Get(2)
	if err != nil || !ok || e.Command != "aws s3 rb s3://b" {
		t.Fatalf("Get(2) = %+v, %v, %v", e, ok, err)
	}
	if _, ok, _ := j.Get(9); ok {
		t.Fatal("Get(9) found an entry")
	}
}
