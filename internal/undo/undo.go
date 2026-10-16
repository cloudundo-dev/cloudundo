// Package undo puts S3 data back the way it was before a protected
// command ran.
//
// Undo never deletes data. It removes delete markers (which only hide
// data) or copies an older version back on top, so an undo can itself be
// undone from the same version history.
package undo

import (
	"context"
	"fmt"
	"sort"
	"strings"

	"github.com/cloudundo/cloudundo/internal/awscli"
	"github.com/cloudundo/cloudundo/internal/journal"
)

// ClientFor returns a CLI client for a Conn.
type ClientFor func(awscli.Conn) awscli.Client

// Action is one restore step.
type Action struct {
	Desc string
	do   func(context.Context) error
}

// Plan is everything undo would do for one journal entry.
type Plan struct {
	Actions []Action
	// Notes explain what will be left as it is, and why.
	Notes []string
}

// Apply runs the actions in order and stops at the first failure.
// It returns how many succeeded.
func (p Plan) Apply(ctx context.Context) (int, error) {
	for i, a := range p.Actions {
		if err := a.do(ctx); err != nil {
			return i, fmt.Errorf("%s: %w", a.Desc, err)
		}
	}
	return len(p.Actions), nil
}

// For builds the plan for an entry without changing anything.
func For(ctx context.Context, e journal.Entry, clientFor ClientFor) (Plan, error) {
	var p Plan
	if len(e.Undo) == 0 {
		switch e.Status {
		case "blocked":
			p.Notes = append(p.Notes, fmt.Sprintf("#%d was blocked, so nothing changed.", e.ID))
		case "undone":
			p.Notes = append(p.Notes, fmt.Sprintf("#%d was already undone.", e.ID))
		default:
			p.Notes = append(p.Notes, fmt.Sprintf("#%d has nothing recorded to undo.", e.ID))
		}
		return p, nil
	}
	if e.Status == "undone" {
		p.Notes = append(p.Notes, fmt.Sprintf("#%d was already undone; running it again only restores what changed since.", e.ID))
	}
	for _, s := range e.Undo {
		c := clientFor(s.Conn)
		var err error
		switch s.Kind {
		case "s3-versions":
			err = planVersions(ctx, c, s, &p)
		case "s3-bucket-copy":
			err = planBucket(ctx, c, s, &p)
		default:
			err = fmt.Errorf("unknown undo step %q (journal from a newer cloudundo?)", s.Kind)
		}
		if err != nil {
			return Plan{}, err
		}
	}
	return p, nil
}

func planVersions(ctx context.Context, c awscli.Client, s journal.UndoStep, p *Plan) error {
	var vs []awscli.Version
	switch {
	case s.Key != "":
		all, err := c.ListVersions(ctx, s.Bucket, s.Key)
		if err != nil {
			return err
		}
		vs = exact(all, s.Key)
	case len(s.Keys) > 0:
		for _, k := range s.Keys {
			all, err := c.ListVersions(ctx, s.Bucket, k)
			if err != nil {
				return err
			}
			vs = append(vs, exact(all, k)...)
		}
	default:
		all, err := c.ListVersions(ctx, s.Bucket, s.Prefix)
		if err != nil {
			return err
		}
		vs = all
	}

	byKey := map[string][]awscli.Version{}
	var keys []string
	for _, v := range vs {
		if _, ok := byKey[v.Key]; !ok {
			keys = append(keys, v.Key)
		}
		byKey[v.Key] = append(byKey[v.Key], v)
	}
	sort.Strings(keys)

	for _, key := range keys {
		hist := byKey[key] // newest first
		sort.SliceStable(hist, func(i, j int) bool { return hist[i].LastModified.After(hist[j].LastModified) })

		var after []awscli.Version
		var before *awscli.Version
		for i := range hist {
			if hist[i].LastModified.After(s.Since) {
				after = append(after, hist[i])
			} else {
				before = &hist[i]
				break
			}
		}
		uri := "s3://" + s.Bucket + "/" + key
		if len(after) == 0 {
			continue // untouched
		}
		if before == nil || before.DeleteMarker {
			p.Notes = append(p.Notes, fmt.Sprintf("%s did not exist before, so it is left in place.", uri))
			continue
		}
		if before.VersionID == "" {
			p.Notes = append(p.Notes, fmt.Sprintf("%s has no version history; cannot restore it.", uri))
			continue
		}

		onlyMarkers := true
		for _, a := range after {
			if !a.DeleteMarker {
				onlyMarkers = false
			}
		}
		if onlyMarkers {
			markers := after
			p.Actions = append(p.Actions, Action{
				Desc: "bring back " + uri,
				do: func(ctx context.Context) error {
					for _, m := range markers {
						if err := c.DeleteVersion(ctx, s.Bucket, key, m.VersionID); err != nil {
							return err
						}
					}
					return nil
				},
			})
			continue
		}
		prev := *before
		p.Actions = append(p.Actions, Action{
			Desc: fmt.Sprintf("restore %s to its version from %s", uri, prev.LastModified.UTC().Format("2006-01-02 15:04:05 UTC")),
			do: func(ctx context.Context) error {
				return c.RestoreVersion(ctx, s.Bucket, key, prev.VersionID)
			},
		})
	}
	return nil
}

func planBucket(ctx context.Context, c awscli.Client, s journal.UndoStep, p *Plan) error {
	rc := c
	rc.C.Region = s.Region
	src := "s3://" + s.BackupBucket + "/" + s.BackupPrefix
	dst := "s3://" + s.Bucket

	if ok, err := rc.BucketExists(ctx, s.BackupBucket); err != nil {
		return err
	} else if !ok {
		return fmt.Errorf("the backup bucket s3://%s is gone, so s3://%s cannot be restored", s.BackupBucket, s.Bucket)
	}
	count, _, err := rc.BucketSummary(ctx, s.BackupBucket, s.BackupPrefix)
	if err != nil {
		return err
	}
	if count == 0 {
		return fmt.Errorf("the backup %s is empty, so s3://%s cannot be restored", src, s.Bucket)
	}

	exists, err := rc.BucketExists(ctx, s.Bucket)
	if err != nil {
		return err
	}
	if exists {
		n, _, err := rc.BucketSummary(ctx, s.Bucket, "")
		if err != nil {
			return err
		}
		if n > 0 {
			p.Notes = append(p.Notes, fmt.Sprintf(
				"%s still exists and has %d objects, so it is left as is. The backup stays at %s.", dst, n, src))
			return nil
		}
		p.Actions = append(p.Actions, Action{
			Desc: fmt.Sprintf("copy %d objects from %s back into %s", count, src, dst),
			do:   func(ctx context.Context) error { return rc.Sync(ctx, src, dst) },
		})
		return nil
	}
	p.Notes = append(p.Notes, fmt.Sprintf(
		"%s comes back private, with its objects but not its old settings (policy, website, CORS, tags).", dst))
	p.Actions = append(p.Actions, Action{
		Desc: fmt.Sprintf("recreate %s in %s", dst, s.Region),
		do:   func(ctx context.Context) error { return createOnly(ctx, rc, s.Bucket, s.Region) },
	}, Action{
		Desc: fmt.Sprintf("copy %d objects from %s back into %s", count, src, dst),
		do:   func(ctx context.Context) error { return rc.Sync(ctx, src, dst) },
	})
	return nil
}

// createOnly recreates a bucket. The original's settings (policy, CORS,
// website, tags) are not restored in this version.
func createOnly(ctx context.Context, c awscli.Client, bucket, region string) error {
	err := c.CreateBucket(ctx, bucket, region)
	if awscli.Has(err, "BucketAlreadyOwnedByYou") {
		return nil
	}
	if awscli.Has(err, "BucketAlreadyExists") {
		return fmt.Errorf("someone else now owns the name %s; restore the backup into another bucket by hand", bucket)
	}
	return err
}

func exact(vs []awscli.Version, key string) []awscli.Version {
	var out []awscli.Version
	for _, v := range vs {
		if v.Key == key {
			out = append(out, v)
		}
	}
	return out
}

// Summary is a short human description of a plan.
func (p Plan) Summary() string {
	var b strings.Builder
	for _, a := range p.Actions {
		b.WriteString("  - " + a.Desc + "\n")
	}
	for _, n := range p.Notes {
		b.WriteString("  note: " + n + "\n")
	}
	return b.String()
}
