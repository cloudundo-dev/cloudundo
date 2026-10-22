// Package effects reads an aws CLI invocation and reports what it would
// do to S3, DynamoDB, RDS or ElastiCache data: delete it, overwrite it,
// or weaken the protections cloudundo relies on. It also spots
// redis-cli FLUSHALL / FLUSHDB.
//
// It only reads the command; it never calls AWS. Deciding how to protect
// each effect is a separate step.
package effects

import (
	"encoding/json"
	"path/filepath"
	"regexp"
	"strings"

	"github.com/cloudundo/cloudundo/internal/awscli"
	"github.com/cloudundo/cloudundo/internal/shellparse"
)

// Kind is what a command does to S3 data.
type Kind string

const (
	// DeleteObjects removes one object, a list of keys, or everything under a prefix.
	DeleteObjects Kind = "delete-objects"
	// DeleteBucket removes the bucket itself. With Force, its objects go first.
	DeleteBucket Kind = "delete-bucket"
	// OverwriteObjects writes over keys that may already hold data.
	OverwriteObjects Kind = "overwrite-objects"
	// WeakenProtection turns off or shortens something undo depends on
	// (versioning suspended, lifecycle rules that expire objects).
	WeakenProtection Kind = "weaken-protection"
)

// Effect is one change to data in AWS.
type Effect struct {
	Kind Kind `json:"kind"`
	// Service is "s3", "dynamodb", "rds", "elasticache" or "redis".
	Service string `json:"service"`
	// Database is the RDS instance, or the Aurora cluster when Cluster.
	Database string `json:"database,omitempty"`
	Cluster  bool   `json:"cluster,omitempty"`
	// Cache is the ElastiCache cluster, replication group or serverless
	// cache named by CacheKind.
	Cache     string `json:"cache,omitempty"`
	CacheKind string `json:"cache_kind,omitempty"`
	// FinalSnapshot is the snapshot the command itself asks AWS to take
	// before deleting; SkipFinalSnapshot means it asked for none.
	FinalSnapshot     string `json:"final_snapshot,omitempty"`
	SkipFinalSnapshot bool   `json:"skip_final_snapshot,omitempty"`
	// TF is set for a terraform apply or destroy (Kind TerraformChange).
	TF *Terraform `json:"terraform,omitempty"`
	// Stack is set for a CloudFormation command (Kind StackChange).
	Stack *Stack `json:"stack,omitempty"`
	// Via is the command that produced a derived effect (a terraform
	// resource address), for messages.
	Via    string `json:"via,omitempty"`
	Bucket string `json:"bucket,omitempty"`
	// Table is the DynamoDB table.
	Table string `json:"table,omitempty"`
	// Items are DynamoDB JSON objects: keys, or whole items when FullItems.
	Items     []string `json:"items,omitempty"`
	FullItems bool     `json:"full_items,omitempty"`
	// Key is set when exactly one object is affected.
	Key string `json:"key,omitempty"`
	// Prefix is set when everything under it may be affected ("" = whole bucket).
	Prefix    string   `json:"prefix,omitempty"`
	Recursive bool     `json:"recursive,omitempty"`
	Keys      []string `json:"keys,omitempty"`
	// Force means a bucket delete also deletes the objects inside.
	Force bool `json:"force,omitempty"`
	// Filtered means --include/--exclude narrow the set; the prefix is an upper bound.
	Filtered bool `json:"filtered,omitempty"`
	// Uncertain means the target could not be read from the command
	// (variables, $(...), xargs, a file:// payload). Treat it as risky.
	Uncertain bool   `json:"uncertain,omitempty"`
	Detail    string `json:"detail,omitempty"`
	Command   string `json:"command"`
	// Conn is the account and region the command uses, so protection
	// and undo run against the same place.
	Conn awscli.Conn `json:"conn"`
}

// Analyze returns the effects of every command in a shell line. It
// follows cd, so a terraform command knows its directory; directories
// are relative to where the line runs.
func Analyze(line string) []Effect {
	var out []Effect
	dir, dirKnown := "", true
	for _, s := range shellparse.Split(line) {
		if len(s.Args) == 0 {
			continue
		}
		switch s.Args[0] {
		case "cd", "pushd":
			switch {
			case len(s.Args) == 1:
				dir = joinDir("", "~")
			case s.Uncertain || looksDynamic(s.Args[1]) || s.Args[1] == "-":
				dirKnown = false
			default:
				dir = joinDir(dir, s.Args[1])
			}
			continue
		case "popd":
			dirKnown = false
			continue
		}
		if effs := terraform(s.Args, dir, dirKnown); effs != nil {
			for i := range effs {
				effs[i].Command = strings.Join(s.Args, " ")
				effs[i].Conn = awscli.ConnFromEnv(s.Env)
				if s.Uncertain {
					effs[i].Uncertain = true
				}
			}
			out = append(out, effs...)
			continue
		}
		if effs := toolDestroy(s.Args); effs != nil {
			out = append(out, effs...)
			continue
		}
		out = append(out, AnalyzeArgs(s.Args, s.Uncertain, s.Env)...)
	}
	return out
}

// AnalyzeArgs returns the S3 effects of one argv. Non-aws commands and
// read-only aws commands return nothing.
func AnalyzeArgs(args []string, uncertain bool, env map[string]string) []Effect {
	if effs := redisCLI(args); effs != nil {
		return effs
	}
	if len(args) == 0 || filepath.Base(args[0]) != "aws" {
		return nil
	}
	p := parseFlags(args[1:])
	if len(p.pos) < 2 {
		return nil
	}
	cmd := strings.Join(args, " ")
	if p.bool("dryrun") {
		return nil
	}

	var effs []Effect
	service := "s3"
	switch p.pos[0] {
	case "s3":
		effs = highLevel(p)
	case "s3api":
		effs = lowLevel(p)
	case "dynamodb":
		service = "dynamodb"
		effs = dynamo(p)
	case "rds":
		service = "rds"
		effs = rds(p)
	case "elasticache":
		service = "elasticache"
		effs = elasticache(p)
	case "cloudformation":
		service = "cloudformation"
		effs = cloudformation(p, args[1:])
	}
	conn := awscli.ConnFromEnv(env)
	conn.Profile = p.value("profile")
	conn.Region = p.value("region")
	conn.Endpoint = p.value("endpoint-url")
	for i := range effs {
		effs[i].Command = cmd
		effs[i].Conn = conn
		if uncertain {
			effs[i].Uncertain = true
		}
		effs[i].Service = service
		if service == "s3" && effs[i].Bucket == "" {
			effs[i].Uncertain = true
		}
		if service == "dynamodb" && !validTable(effs[i].Table) {
			effs[i].Uncertain = true
		}
		if service == "rds" && !validName(effs[i].Database) {
			effs[i].Uncertain = true
		}
		if service == "elasticache" && !validName(effs[i].Cache) {
			effs[i].Uncertain = true
		}
	}
	return effs
}

// highLevel handles "aws s3 <verb>".
func highLevel(p parsed) []Effect {
	verb := p.pos[1]
	args := p.pos[2:]
	recursive := p.bool("recursive")
	filtered := p.has("include") || p.has("exclude")
	arg := func(i int) (s3URI, argKind) {
		if i >= len(args) {
			return s3URI{}, argMissing
		}
		return classify(args[i])
	}
	hidden := func(k Kind) []Effect {
		return []Effect{{Kind: k, Uncertain: true, Detail: "the target is not written in the command"}}
	}

	switch verb {
	case "rm":
		u, k := arg(0)
		switch k {
		case argS3:
			return []Effect{objectsEffect(DeleteObjects, u, recursive, filtered)}
		case argMissing, argDynamic:
			return hidden(DeleteObjects)
		}
		return nil // not an S3 path: aws rejects it

	case "rb":
		u, k := arg(0)
		switch k {
		case argS3:
			return []Effect{{Kind: DeleteBucket, Bucket: u.bucket, Force: p.bool("force"), Uncertain: u.uncertain}}
		case argMissing, argDynamic:
			return hidden(DeleteBucket)
		}
		return nil

	case "mv", "cp", "sync":
		src, sk := arg(0)
		dst, dk := arg(1)
		var effs []Effect
		if verb == "mv" {
			switch sk {
			case argS3:
				e := objectsEffect(DeleteObjects, src, recursive, filtered)
				e.Detail = "moved away"
				effs = append(effs, e)
			case argMissing, argDynamic:
				effs = append(effs, hidden(DeleteObjects)...)
			}
		}
		switch dk {
		case argS3:
			if verb == "sync" {
				effs = append(effs, Effect{
					Kind: OverwriteObjects, Bucket: dst.bucket, Prefix: dirPrefix(dst.key),
					Recursive: true, Filtered: filtered, Uncertain: dst.uncertain,
				})
				if p.bool("delete") {
					effs = append(effs, Effect{
						Kind: DeleteObjects, Bucket: dst.bucket, Prefix: dirPrefix(dst.key),
						Recursive: true, Filtered: filtered, Uncertain: dst.uncertain,
						Detail: "sync --delete removes objects missing from the source",
					})
				}
			} else {
				srcText := ""
				if sk != argMissing {
					srcText = args[0]
				}
				effs = append(effs, destEffect(dst, srcText, recursive, filtered))
			}
		case argMissing, argDynamic:
			effs = append(effs, hidden(OverwriteObjects)...)
		}
		return effs
	}
	return nil
}

type argKind int

const (
	argMissing argKind = iota
	argS3              // s3://...
	argDynamic         // $VAR, $(...), backticks: could be anything
	argLocal           // a local path
)

func classify(a string) (s3URI, argKind) {
	if u, ok := parseS3URI(a); ok {
		return u, argS3
	}
	if strings.ContainsAny(a, "$`") {
		return s3URI{}, argDynamic
	}
	return s3URI{}, argLocal
}

// lowLevel handles "aws s3api <operation>".
func lowLevel(p parsed) []Effect {
	op := p.pos[1]
	bucket := p.value("bucket")
	key := p.value("key")
	bucketUncertain := looksDynamic(bucket)

	switch op {
	case "delete-object":
		if p.has("version-id") {
			return []Effect{{Kind: WeakenProtection, Bucket: bucket, Key: key, Uncertain: bucketUncertain,
				Detail: "permanently deletes an object version"}}
		}
		return []Effect{{Kind: DeleteObjects, Bucket: bucket, Key: key, Uncertain: bucketUncertain || key == "" || looksDynamic(key)}}

	case "delete-objects":
		if strings.Contains(p.value("delete"), "VersionId") {
			return []Effect{{Kind: WeakenProtection, Bucket: bucket, Uncertain: bucketUncertain,
				Detail: "permanently deletes object versions"}}
		}
		keys, ok := parseDeleteKeys(p.value("delete"))
		e := Effect{Kind: DeleteObjects, Bucket: bucket, Keys: keys, Uncertain: bucketUncertain || !ok}
		if !ok {
			e.Detail = "could not read the key list"
		}
		return []Effect{e}

	case "delete-bucket":
		return []Effect{{Kind: DeleteBucket, Bucket: bucket, Uncertain: bucketUncertain,
			Detail: "AWS refuses this while the bucket still has objects"}}

	case "put-object", "copy-object":
		return []Effect{{Kind: OverwriteObjects, Bucket: bucket, Key: key, Uncertain: bucketUncertain || key == ""}}

	case "put-bucket-versioning":
		cfg := p.value("versioning-configuration")
		if strings.Contains(strings.ToLower(cfg), "suspended") {
			return []Effect{{Kind: WeakenProtection, Bucket: bucket, Uncertain: bucketUncertain,
				Detail: "suspends versioning, so later deletes cannot be undone"}}
		}
		return nil

	case "put-bucket-lifecycle-configuration", "put-bucket-lifecycle":
		return []Effect{{Kind: WeakenProtection, Bucket: bucket, Uncertain: bucketUncertain,
			Detail: "lifecycle rules can expire objects and old versions"}}

	case "delete-bucket-lifecycle":
		// Removing rules keeps more data, not less.
		return nil
	}
	return nil
}

func objectsEffect(k Kind, u s3URI, recursive, filtered bool) Effect {
	e := Effect{Kind: k, Bucket: u.bucket, Filtered: filtered, Uncertain: u.uncertain}
	if recursive {
		e.Recursive = true
		e.Prefix = u.key
	} else {
		e.Key = u.key
		if u.key == "" {
			e.Uncertain = true
			e.Detail = "no object key"
		}
	}
	return e
}

// destEffect describes writing to dst. For a single-file copy into a
// "folder" URI (ending in /), the final key depends on the source name.
func destEffect(dst s3URI, src string, recursive, filtered bool) Effect {
	e := Effect{Kind: OverwriteObjects, Bucket: dst.bucket, Filtered: filtered, Uncertain: dst.uncertain}
	switch {
	case recursive:
		e.Recursive = true
		e.Prefix = dirPrefix(dst.key)
	case dst.key == "" || strings.HasSuffix(dst.key, "/"):
		name := src
		if su, ok := parseS3URI(src); ok {
			name = su.key
		}
		e.Key = dst.key + filepath.Base(name)
		if name == "" || looksDynamic(name) {
			e.Uncertain = true
		}
	default:
		e.Key = dst.key
	}
	return e
}

// dirPrefix normalises a prefix used as a folder ("a/b" and "a/b/" both
// mean everything under a/b/). An empty prefix means the whole bucket.
func dirPrefix(k string) string {
	if k == "" || strings.HasSuffix(k, "/") {
		return k
	}
	return k + "/"
}

type s3URI struct {
	bucket    string
	key       string
	uncertain bool
}

func parseS3URI(s string) (s3URI, bool) {
	if !strings.HasPrefix(s, "s3://") {
		return s3URI{}, false
	}
	rest := strings.TrimPrefix(s, "s3://")
	u := s3URI{uncertain: looksDynamic(rest)}
	// Access point and Outposts ARNs: the bucket is not a plain name.
	if strings.HasPrefix(rest, "arn:") {
		u.uncertain = true
		u.bucket = ""
		return u, true
	}
	if i := strings.IndexByte(rest, '/'); i >= 0 {
		u.bucket, u.key = rest[:i], rest[i+1:]
	} else {
		u.bucket = rest
	}
	return u, true
}

func looksDynamic(s string) bool {
	return strings.ContainsAny(s, "$`*")
}

var shorthandKey = regexp.MustCompile(`Key=([^,}\]\s]+)`)

// parseDeleteKeys reads the --delete argument of s3api delete-objects in
// JSON or shorthand form. file:// and fileb:// payloads are not read.
func parseDeleteKeys(v string) ([]string, bool) {
	if v == "" || strings.HasPrefix(v, "file://") || strings.HasPrefix(v, "fileb://") || looksDynamic(v) {
		return nil, false
	}
	var payload struct {
		Objects []struct {
			Key string `json:"Key"`
		} `json:"Objects"`
	}
	if strings.HasPrefix(strings.TrimSpace(v), "{") {
		if err := json.Unmarshal([]byte(v), &payload); err != nil {
			return nil, false
		}
		var keys []string
		for _, o := range payload.Objects {
			keys = append(keys, o.Key)
		}
		return keys, len(keys) > 0
	}
	var keys []string
	for _, m := range shorthandKey.FindAllStringSubmatch(v, -1) {
		keys = append(keys, m[1])
	}
	return keys, len(keys) > 0
}

// parsed holds positional arguments and flags of an aws command.
type parsed struct {
	pos   []string
	flags map[string]string
	seen  map[string]bool
}

// booleanFlags never take a value. Every other --flag takes the next word.
var booleanFlags = map[string]bool{
	"recursive": true, "force": true, "dryrun": true, "quiet": true,
	"only-show-errors": true, "no-progress": true, "debug": true,
	"no-verify-ssl": true, "no-paginate": true, "no-sign-request": true,
	"delete": true, "exact-timestamps": true, "size-only": true,
	"follow-symlinks": true, "no-follow-symlinks": true,
	"no-guess-mime-type": true, "ignore-glacier-warnings": true,
	"force-glacier-transfer": true, "bypass-governance-retention": true,
	"no-bypass-governance-retention": true, "no-cli-pager": true,
	"cli-auto-prompt": true, "no-cli-auto-prompt": true,
	"deletion-protection-enabled": true, "no-deletion-protection-enabled": true,
	"consistent-read": true, "no-consistent-read": true,
	// rds and elasticache
	"skip-final-snapshot": true, "no-skip-final-snapshot": true,
	"delete-automated-backups": true, "no-delete-automated-backups": true,
	"deletion-protection": true, "no-deletion-protection": true,
	"apply-immediately": true, "no-apply-immediately": true,
	"multi-az": true, "no-multi-az": true,
	"publicly-accessible": true, "no-publicly-accessible": true,
	"retain-primary-cluster": true, "no-retain-primary-cluster": true,
	"copy-tags-to-snapshot": true, "no-copy-tags-to-snapshot": true,
}

// s3apiValueFlags are s3api flags whose name collides with a boolean
// flag of "aws s3" ("--delete" is a JSON payload for delete-objects).
var s3apiValueFlags = map[string]bool{"delete": true}

func parseFlags(args []string) parsed {
	p := parsed{flags: map[string]string{}, seen: map[string]bool{}}
	isS3api := len(args) > 0 && args[0] == "s3api"
	for i := 0; i < len(args); i++ {
		a := args[i]
		if !strings.HasPrefix(a, "--") || a == "--" {
			p.pos = append(p.pos, a)
			continue
		}
		name := strings.TrimPrefix(a, "--")
		if eq := strings.IndexByte(name, '='); eq >= 0 {
			p.flags[name[:eq]] = name[eq+1:]
			p.seen[name[:eq]] = true
			continue
		}
		p.seen[name] = true
		takesValue := !booleanFlags[name] || (isS3api && s3apiValueFlags[name])
		if takesValue && i+1 < len(args) && !strings.HasPrefix(args[i+1], "--") {
			p.flags[name] = args[i+1]
			i++
		}
	}
	return p
}

func (p parsed) bool(name string) bool    { return p.seen[name] }
func (p parsed) has(name string) bool     { return p.seen[name] }
func (p parsed) value(name string) string { return p.flags[name] }
