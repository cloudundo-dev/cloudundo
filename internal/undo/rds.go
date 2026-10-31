package undo

import (
	"context"
	"fmt"

	"github.com/cloudundo-dev/cloudundo/internal/awscli"
	"github.com/cloudundo-dev/cloudundo/internal/journal"
)

// planDB restores a deleted RDS database from its snapshot with the
// recorded settings, then, once it is available, puts back what a restore
// cannot set (backup retention, an Aurora cluster's instances). Like a
// table restore, it can take several undo runs.
func planDB(ctx context.Context, c awscli.Client, s *journal.UndoStep, p *Plan) error {
	if s.DB == nil || s.Snapshot == "" {
		return fmt.Errorf("the journal entry has no snapshot to restore from")
	}
	d := *s.DB
	what := "database " + d.ID
	if d.Cluster {
		what = "Aurora cluster " + d.ID
	}
	info, exists, err := c.DescribeDB(ctx, d.ID, d.Cluster)
	if err != nil {
		return err
	}

	switch {
	case !exists:
		snaps, err := c.DBSnapshots(ctx, d.ID, s.Snapshot, d.Cluster)
		if err != nil {
			return err
		}
		if len(snaps) == 0 {
			return fmt.Errorf("snapshot %s of %s is gone, so it cannot be restored", s.Snapshot, what)
		}
		if snaps[0].Status != "available" {
			p.Notes = append(p.Notes, fmt.Sprintf("Snapshot %s is still being made (%s). Run the same undo again in a few minutes.", s.Snapshot, snaps[0].Status))
			p.Pending = true
			return nil
		}
		p.Actions = append(p.Actions, Action{
			Desc: fmt.Sprintf("restore %s from snapshot %s with its old settings", what, s.Snapshot),
			do: func(ctx context.Context) error {
				arn, err := c.RestoreDB(ctx, d, s.Snapshot)
				if err != nil {
					return err
				}
				s.RestoredID = arn
				s.SettingsApplied = false
				return nil
			},
		})
		p.Notes = append(p.Notes, fmt.Sprintf("Restoring %s takes several minutes, longer for a big database. Run the same undo again afterwards to finish.", what))
		p.Pending = true
		return nil

	case s.RestoredID == "" && info.Status == "deleting":
		p.Notes = append(p.Notes, fmt.Sprintf("%s is still being deleted. Run the same undo again in a few minutes.", what))
		p.Pending = true
		return nil

	case s.RestoredID == "":
		p.Notes = append(p.Notes, fmt.Sprintf("%s still exists, so there is nothing to restore. Snapshot %s is kept.", what, s.Snapshot))
		return nil

	case info.ResourceID != s.RestoredID:
		p.Notes = append(p.Notes, fmt.Sprintf("A different %s exists now, so it is left alone. Snapshot %s is kept.", what, s.Snapshot))
		return nil

	case s.SettingsApplied:
		return nil

	case info.Status != "available":
		p.Notes = append(p.Notes, fmt.Sprintf("%s is still being restored (%s). Run the same undo again in a few minutes.", what, info.Status))
		p.Pending = true
		return nil
	}

	if d.BackupRetention > 0 && info.BackupRetention != d.BackupRetention {
		days := d.BackupRetention
		p.Actions = append(p.Actions, Action{
			Desc: fmt.Sprintf("keep %d days of automated backups for %s again", days, d.ID),
			do:   func(ctx context.Context) error { return c.SetBackupRetention(ctx, d, days) },
		})
	}
	if d.Cluster {
		have := map[string]bool{}
		for _, m := range info.Members {
			have[m.ID] = true
		}
		for _, m := range d.Members {
			if have[m.ID] {
				continue
			}
			if m.Class == "" {
				p.Notes = append(p.Notes, fmt.Sprintf("The size of instance %s was not recorded; add it to the cluster yourself.", m.ID))
				continue
			}
			m := m
			p.Actions = append(p.Actions, Action{
				Desc: fmt.Sprintf("add instance %s (%s) back to %s", m.ID, m.Class, d.ID),
				do:   func(ctx context.Context) error { return c.CreateClusterInstance(ctx, d, m) },
			})
		}
		if len(d.Members) == 0 {
			p.Notes = append(p.Notes, fmt.Sprintf("%s had no instances when it was deleted; add one to use it.", what))
		}
	}
	p.Notes = append(p.Notes, fmt.Sprintf("%s keeps its name, so apps reach it at the same address. Monitoring, IAM roles and log exports are not restored.", what))
	p.Actions = append(p.Actions, Action{
		Desc: "mark " + d.ID + " as fully restored",
		do: func(context.Context) error {
			s.SettingsApplied = true
			return nil
		},
	})
	return nil
}
