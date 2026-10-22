// Package awstest is an in-memory stand-in for the aws CLI, covering the
// S3 calls cloudundo makes. It keeps object versions and delete markers
// the way S3 does, so protect and undo can be tested end to end.
package awstest

import (
	"context"
	"encoding/json"
	"fmt"
	"net/url"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/cloudundo/cloudundo/internal/awscli"
)

type version struct {
	id     string
	data   string
	marker bool
	at     time.Time
}

type table struct {
	id         string
	arn        string
	status     string
	keyAttrs   []string
	items      map[string]string // canonical key -> item JSON
	ttl        string
	tags       []map[string]string
	pitr       bool
	stream     string
	protection bool
	// creatingPolls is how many more describe-table calls report CREATING.
	creatingPolls int
}

type backup struct {
	table *table
	name  string
	polls int // describe-backup calls left before AVAILABLE
}

type bucket struct {
	region     string
	versioning string // "", "Enabled", "Suspended"
	lifecycle  *string
	objects    map[string][]version // oldest first
}

// Fake implements awscli.Runner.
type Fake struct {
	mu      sync.Mutex
	buckets map[string]*bucket
	tables  map[string]*table
	backups map[string]*backup
	// BackupPolls is how many describe-backup calls report CREATING for a
	// new backup; RestorePolls the same for a restored table.
	BackupPolls  int
	RestorePolls int
	clock        time.Time
	nextID       int
	Account      string
	// Calls records every call, global flags removed.
	Calls [][]string
	// Env records the extra environment of every call.
	Env [][]string
	// Fail makes any call whose joined args contain the key fail with the value as stderr.
	Fail map[string]string

	dbs, clusters map[string]*fdb
	dbSnaps       map[string]*fsnap
	caches        map[string]*fcache
	stacks        map[string]*fstack
	changeSets    map[string]*fchangeSet
	deployChanges map[string][]FakeChange
	// SnapshotPolls is how many describe calls report a new RDS snapshot
	// as "creating".
	SnapshotPolls int
}

// New returns an empty fake account.
func New() *Fake {
	return &Fake{
		buckets: map[string]*bucket{},
		tables:  map[string]*table{},
		backups: map[string]*backup{},
		clock:   time.Date(2026, 10, 18, 9, 0, 0, 0, time.UTC),
		Account: "123456789012",
		Fail:    map[string]string{},
	}
}

func (f *Fake) tick() time.Time {
	f.clock = f.clock.Add(time.Second)
	return f.clock
}

// Now returns the fake clock; the next change happens after it.
// SetClock moves the fake's clock, for tests that run cloudundo as a
// separate program, which reads the real time.
func (f *Fake) SetClock(t time.Time) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.clock = t
}

func (f *Fake) Now() time.Time {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.clock
}

// CreateBucket adds a bucket for a test.
func (f *Fake) CreateBucket(name, region, versioning string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.buckets[name] = &bucket{region: region, versioning: versioning, objects: map[string][]version{}}
}

// SetLifecycle sets raw lifecycle JSON for a test.
func (f *Fake) SetLifecycle(name, raw string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.buckets[name].lifecycle = &raw
}

// Lifecycle returns the raw lifecycle JSON, or "".
func (f *Fake) Lifecycle(name string) string {
	f.mu.Lock()
	defer f.mu.Unlock()
	b := f.buckets[name]
	if b == nil || b.lifecycle == nil {
		return ""
	}
	return *b.lifecycle
}

// Versioning returns a bucket's versioning status.
func (f *Fake) Versioning(name string) string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.buckets[name].versioning
}

// HasBucket reports whether the bucket exists.
func (f *Fake) HasBucket(name string) bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.buckets[name] != nil
}

// Put writes an object, as an app or agent would.
func (f *Fake) Put(b, key, data string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.put(f.buckets[b], key, data)
}

func (f *Fake) put(b *bucket, key, data string) {
	v := version{id: "null", data: data, at: f.tick()}
	if b.versioning == "Enabled" {
		f.nextID++
		v.id = fmt.Sprintf("v%d", f.nextID)
		b.objects[key] = append(b.objects[key], v)
		return
	}
	// Unversioned or suspended: the "null" version is replaced.
	vs := b.objects[key]
	kept := vs[:0]
	for _, old := range vs {
		if old.id != "null" {
			kept = append(kept, old)
		}
	}
	b.objects[key] = append(kept, v)
}

// Delete removes an object the way "aws s3 rm" does.
func (f *Fake) Delete(b, key string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.del(f.buckets[b], key)
}

func (f *Fake) del(b *bucket, key string) {
	if b.versioning == "Enabled" {
		f.nextID++
		b.objects[key] = append(b.objects[key], version{id: fmt.Sprintf("dm%d", f.nextID), marker: true, at: f.tick()})
		return
	}
	f.tick()
	delete(b.objects, key)
}

// DeletePrefix deletes every current object under prefix ("aws s3 rm --recursive").
func (f *Fake) DeletePrefix(b, prefix string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	bk := f.buckets[b]
	for _, k := range f.currentKeys(bk) {
		if strings.HasPrefix(k, prefix) {
			f.del(bk, k)
		}
	}
}

// RemoveBucket deletes the bucket and everything in it ("aws s3 rb --force").
func (f *Fake) RemoveBucket(name string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.tick()
	delete(f.buckets, name)
}

// Get returns the current content of an object.
func (f *Fake) Get(b, key string) (string, bool) {
	f.mu.Lock()
	defer f.mu.Unlock()
	bk := f.buckets[b]
	if bk == nil {
		return "", false
	}
	vs := bk.objects[key]
	if len(vs) == 0 || vs[len(vs)-1].marker {
		return "", false
	}
	return vs[len(vs)-1].data, true
}

func (f *Fake) currentKeys(b *bucket) []string {
	var keys []string
	for k, vs := range b.objects {
		if len(vs) > 0 && !vs[len(vs)-1].marker {
			keys = append(keys, k)
		}
	}
	sort.Strings(keys)
	return keys
}

// Run implements awscli.Runner.
func (f *Fake) Run(_ context.Context, env []string, args ...string) ([]byte, error) {
	f.mu.Lock()
	defer f.mu.Unlock()

	var clean []string
	flags := map[string]string{}
	for i := 0; i < len(args); i++ {
		a := args[i]
		if strings.HasPrefix(a, "--") && a != "--only-show-errors" {
			name := strings.TrimPrefix(a, "--")
			if i+1 < len(args) && !strings.HasPrefix(args[i+1], "--") {
				flags[name] = args[i+1]
				i++
			} else {
				flags[name] = "true" // a boolean flag
			}
			continue
		}
		clean = append(clean, a)
	}
	f.Calls = append(f.Calls, append([]string{}, args...))
	f.Env = append(f.Env, env)

	joined := strings.Join(args, " ")
	for k, msg := range f.Fail {
		if strings.Contains(joined, k) {
			return nil, &awscli.Error{Args: args, Code: 254, Stderr: msg}
		}
	}

	errOut := func(msg string) ([]byte, error) {
		return nil, &awscli.Error{Args: args, Code: 254, Stderr: msg}
	}
	js := func(v any) ([]byte, error) { return json.Marshal(v) }
	if len(clean) < 2 {
		return errOut("usage")
	}
	switch clean[0] {
	case "dynamodb":
		return f.dynamo(clean[1], flags, args)
	case "rds":
		return f.rdsCall(clean[1], flags, args)
	case "elasticache":
		return f.cacheCall(clean[1], flags, args)
	case "cloudformation":
		return f.cfnCall(clean[1], flags, args)
	}
	name := flags["bucket"]
	b := f.buckets[name]
	needBucket := func() bool { return b != nil }
	noBucket := "An error occurred (NoSuchBucket) when calling the operation: The specified bucket does not exist"

	switch clean[0] + " " + clean[1] {
	case "sts get-caller-identity":
		return js(map[string]string{"Account": f.Account})

	case "s3api head-bucket":
		if !needBucket() {
			return errOut("An error occurred (404) when calling the HeadBucket operation: Not Found")
		}
		return nil, nil

	case "s3api get-bucket-location":
		if !needBucket() {
			return errOut(noBucket)
		}
		if b.region == "us-east-1" {
			return js(map[string]any{"LocationConstraint": nil})
		}
		return js(map[string]any{"LocationConstraint": b.region})

	case "s3api get-bucket-versioning":
		if !needBucket() {
			return errOut(noBucket)
		}
		if b.versioning == "" {
			return []byte("{}"), nil
		}
		return js(map[string]string{"Status": b.versioning})

	case "s3api put-bucket-versioning":
		if !needBucket() {
			return errOut(noBucket)
		}
		if strings.Contains(flags["versioning-configuration"], "Enabled") {
			b.versioning = "Enabled"
		} else {
			b.versioning = "Suspended"
		}
		return nil, nil

	case "s3api get-bucket-lifecycle-configuration":
		if !needBucket() {
			return errOut(noBucket)
		}
		if b.lifecycle == nil {
			return errOut("An error occurred (NoSuchLifecycleConfiguration) when calling the GetBucketLifecycleConfiguration operation: The lifecycle configuration does not exist")
		}
		return []byte(*b.lifecycle), nil

	case "s3api put-bucket-lifecycle-configuration":
		if !needBucket() {
			return errOut(noBucket)
		}
		var check struct{ Rules []json.RawMessage }
		if err := json.Unmarshal([]byte(flags["lifecycle-configuration"]), &check); err != nil {
			return errOut("Invalid JSON: " + err.Error())
		}
		raw := flags["lifecycle-configuration"]
		b.lifecycle = &raw
		return nil, nil

	case "s3api list-object-versions":
		if !needBucket() {
			return errOut(noBucket)
		}
		type entry struct {
			Key          string
			VersionId    string
			IsLatest     bool
			LastModified time.Time
			Size         int64 `json:",omitempty"`
		}
		var out struct {
			Versions      []entry `json:",omitempty"`
			DeleteMarkers []entry `json:",omitempty"`
		}
		for k, vs := range b.objects {
			if !strings.HasPrefix(k, flags["prefix"]) {
				continue
			}
			for i := len(vs) - 1; i >= 0; i-- {
				e := entry{k, vs[i].id, i == len(vs)-1, vs[i].at, int64(len(vs[i].data))}
				if vs[i].marker {
					e.Size = 0
					out.DeleteMarkers = append(out.DeleteMarkers, e)
				} else {
					out.Versions = append(out.Versions, e)
				}
			}
		}
		return js(out)

	case "s3api delete-objects":
		if !needBucket() {
			return errOut(noBucket)
		}
		var req struct {
			Objects []struct{ Key, VersionId string }
		}
		if err := json.Unmarshal([]byte(flags["delete"]), &req); err != nil {
			return errOut("Invalid JSON: " + err.Error())
		}
		for _, o := range req.Objects {
			vs := b.objects[o.Key]
			for i, v := range vs {
				if v.id == o.VersionId {
					b.objects[o.Key] = append(vs[:i:i], vs[i+1:]...)
					break
				}
			}
			if len(b.objects[o.Key]) == 0 {
				delete(b.objects, o.Key)
			}
		}
		f.tick()
		return js(map[string]any{})

	case "s3api delete-object":
		if !needBucket() {
			return errOut(noBucket)
		}
		key, vid := flags["key"], flags["version-id"]
		if vid == "" {
			f.del(b, key)
			return nil, nil
		}
		vs := b.objects[key]
		for i, v := range vs {
			if v.id == vid {
				b.objects[key] = append(vs[:i:i], vs[i+1:]...)
				f.tick()
				return nil, nil
			}
		}
		return errOut("An error occurred (NoSuchVersion)")

	case "s3api copy-object":
		if !needBucket() {
			return errOut(noBucket)
		}
		src := flags["copy-source"]
		q := ""
		if i := strings.IndexByte(src, '?'); i >= 0 {
			src, q = src[:i], src[i+1:]
		}
		slash := strings.IndexByte(src, '/')
		sb, rawKey := src[:slash], src[slash+1:]
		skey, err := url.PathUnescape(rawKey)
		if err != nil {
			return errOut("bad copy source")
		}
		vals, _ := url.ParseQuery(q)
		srcB := f.buckets[sb]
		if srcB == nil {
			return errOut(noBucket)
		}
		for _, v := range srcB.objects[skey] {
			if v.id == vals.Get("versionId") && !v.marker {
				f.put(b, flags["key"], v.data)
				return js(map[string]any{"CopyObjectResult": map[string]string{"ETag": "x"}})
			}
		}
		return errOut("An error occurred (NoSuchVersion)")

	case "s3api list-objects-v2":
		if !needBucket() {
			return errOut(noBucket)
		}
		var size int64
		n := 0
		for _, k := range f.currentKeys(b) {
			if !strings.HasPrefix(k, flags["prefix"]) {
				continue
			}
			vs := b.objects[k]
			size += int64(len(vs[len(vs)-1].data))
			n++
		}
		return js(map[string]any{"count": n, "size": size})

	case "s3api create-bucket":
		if b != nil {
			return errOut("An error occurred (BucketAlreadyOwnedByYou)")
		}
		region := "us-east-1"
		if lc := flags["create-bucket-configuration"]; lc != "" {
			region = strings.TrimPrefix(lc, "LocationConstraint=")
		}
		f.buckets[name] = &bucket{region: region, objects: map[string][]version{}}
		return js(map[string]string{"Location": "/" + name})

	case "s3api put-public-access-block":
		if !needBucket() {
			return errOut(noBucket)
		}
		return nil, nil

	case "s3 sync":
		if len(clean) < 4 {
			return errOut("usage: sync src dst")
		}
		sb, sp := splitURI(clean[2])
		db, dp := splitURI(clean[3])
		srcB, dstB := f.buckets[sb], f.buckets[db]
		if srcB == nil || dstB == nil {
			return errOut(noBucket)
		}
		for _, k := range f.currentKeys(srcB) {
			if strings.HasPrefix(k, sp) {
				vs := srcB.objects[k]
				f.put(dstB, dp+strings.TrimPrefix(k, sp), vs[len(vs)-1].data)
			}
		}
		return nil, nil
	}
	return errOut("fake aws: unsupported call " + joined)
}

func splitURI(u string) (string, string) {
	rest := strings.TrimPrefix(u, "s3://")
	if i := strings.IndexByte(rest, '/'); i >= 0 {
		return rest[:i], rest[i+1:]
	}
	return rest, ""
}

// Count returns how many versions and delete markers are under prefix.
func (f *Fake) Count(b, prefix string) int {
	f.mu.Lock()
	defer f.mu.Unlock()
	n := 0
	if bk := f.buckets[b]; bk != nil {
		for k, vs := range bk.objects {
			if strings.HasPrefix(k, prefix) {
				n += len(vs)
			}
		}
	}
	return n
}
