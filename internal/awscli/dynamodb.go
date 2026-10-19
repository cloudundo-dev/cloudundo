package awscli

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"strings"
)

// TableInfo is what cloudundo needs from describe-table.
type TableInfo struct {
	// ID is the table's unique TableId; a table re-created or restored
	// under the same name gets a new one, while the ARN stays the same.
	ID                 string
	Arn                string
	Status             string
	KeyAttrs           []string
	DeletionProtection bool
	StreamViewType     string // "" when streams are off
}

// DescribeTable returns the table, or ok=false if it does not exist.
func (c Client) DescribeTable(ctx context.Context, table string) (TableInfo, bool, error) {
	out, err := c.call(ctx, "dynamodb", "describe-table", "--table-name", table)
	if Has(err, "ResourceNotFoundException") {
		return TableInfo{}, false, nil
	}
	if err != nil {
		return TableInfo{}, false, err
	}
	var r struct {
		Table struct {
			TableId                   string
			TableArn                  string
			TableStatus               string
			KeySchema                 []struct{ AttributeName string }
			DeletionProtectionEnabled bool
			StreamSpecification       *struct {
				StreamEnabled  bool
				StreamViewType string
			}
		}
	}
	if err := json.Unmarshal(out, &r); err != nil {
		return TableInfo{}, false, fmt.Errorf("read table %s: %w", table, err)
	}
	t := TableInfo{ID: r.Table.TableId, Arn: r.Table.TableArn, Status: r.Table.TableStatus, DeletionProtection: r.Table.DeletionProtectionEnabled}
	for _, k := range r.Table.KeySchema {
		t.KeyAttrs = append(t.KeyAttrs, k.AttributeName)
	}
	if s := r.Table.StreamSpecification; s != nil && s.StreamEnabled {
		t.StreamViewType = s.StreamViewType
	}
	return t, true, nil
}

// GetItem returns the item as compact DynamoDB JSON, or "" if there is none.
func (c Client) GetItem(ctx context.Context, table, key string) (string, error) {
	out, err := c.call(ctx, "dynamodb", "get-item", "--table-name", table, "--key", key, "--consistent-read")
	if err != nil {
		return "", err
	}
	var r struct{ Item json.RawMessage }
	if len(bytes.TrimSpace(out)) > 0 {
		if err := json.Unmarshal(out, &r); err != nil {
			return "", fmt.Errorf("read item from %s: %w", table, err)
		}
	}
	if len(r.Item) == 0 || string(r.Item) == "null" {
		return "", nil
	}
	var b bytes.Buffer
	if err := json.Compact(&b, r.Item); err != nil {
		return "", err
	}
	return b.String(), nil
}

// PutItem writes item (DynamoDB JSON).
func (c Client) PutItem(ctx context.Context, table, item string) error {
	_, err := c.call(ctx, "dynamodb", "put-item", "--table-name", table, "--item", item)
	return err
}

// CreateBackup starts an on-demand backup and returns its ARN and status.
func (c Client) CreateBackup(ctx context.Context, table, name string) (string, string, error) {
	out, err := c.call(ctx, "dynamodb", "create-backup", "--table-name", table, "--backup-name", name)
	if err != nil {
		return "", "", err
	}
	var r struct {
		BackupDetails struct{ BackupArn, BackupStatus string }
	}
	if err := json.Unmarshal(out, &r); err != nil || r.BackupDetails.BackupArn == "" {
		return "", "", fmt.Errorf("read backup of %s: %v", table, err)
	}
	return r.BackupDetails.BackupArn, r.BackupDetails.BackupStatus, nil
}

// BackupStatus returns CREATING, AVAILABLE or DELETED.
func (c Client) BackupStatus(ctx context.Context, arn string) (string, error) {
	out, err := c.call(ctx, "dynamodb", "describe-backup", "--backup-arn", arn)
	if err != nil {
		return "", err
	}
	var r struct {
		BackupDescription struct {
			BackupDetails struct{ BackupStatus string }
		}
	}
	if err := json.Unmarshal(out, &r); err != nil {
		return "", fmt.Errorf("read backup %s: %w", arn, err)
	}
	return r.BackupDescription.BackupDetails.BackupStatus, nil
}

// RestoreTable creates table from a backup and returns the new table's id.
func (c Client) RestoreTable(ctx context.Context, table, backupArn string) (string, error) {
	out, err := c.call(ctx, "dynamodb", "restore-table-from-backup", "--target-table-name", table, "--backup-arn", backupArn)
	if err != nil {
		return "", err
	}
	var r struct {
		TableDescription struct{ TableId string }
	}
	if err := json.Unmarshal(out, &r); err != nil {
		return "", fmt.Errorf("read restored table %s: %w", table, err)
	}
	return r.TableDescription.TableId, nil
}

// TTL returns the TTL attribute, or "" when TTL is off.
func (c Client) TTL(ctx context.Context, table string) (string, error) {
	out, err := c.call(ctx, "dynamodb", "describe-time-to-live", "--table-name", table)
	if err != nil {
		return "", err
	}
	var r struct {
		TimeToLiveDescription struct{ TimeToLiveStatus, AttributeName string }
	}
	if err := json.Unmarshal(out, &r); err != nil {
		return "", fmt.Errorf("read TTL of %s: %w", table, err)
	}
	if s := r.TimeToLiveDescription.TimeToLiveStatus; s == "ENABLED" || s == "ENABLING" {
		return r.TimeToLiveDescription.AttributeName, nil
	}
	return "", nil
}

// SetTTL turns TTL on for attr.
func (c Client) SetTTL(ctx context.Context, table, attr string) error {
	_, err := c.call(ctx, "dynamodb", "update-time-to-live", "--table-name", table,
		"--time-to-live-specification", "Enabled=true,AttributeName="+attr)
	return err
}

// Tag is a DynamoDB resource tag.
type Tag struct {
	Key   string `json:"Key"`
	Value string `json:"Value"`
}

// Tags returns the tags of a table ARN.
func (c Client) Tags(ctx context.Context, arn string) ([]Tag, error) {
	out, err := c.call(ctx, "dynamodb", "list-tags-of-resource", "--resource-arn", arn)
	if err != nil {
		return nil, err
	}
	var r struct{ Tags []Tag }
	if err := json.Unmarshal(out, &r); err != nil {
		return nil, fmt.Errorf("read tags of %s: %w", arn, err)
	}
	return r.Tags, nil
}

// TagResource adds tags to a table ARN.
func (c Client) TagResource(ctx context.Context, arn string, tags []Tag) error {
	body, err := json.Marshal(tags)
	if err != nil {
		return err
	}
	_, err = c.call(ctx, "dynamodb", "tag-resource", "--resource-arn", arn, "--tags", string(body))
	return err
}

// PITR reports whether point-in-time recovery is on.
func (c Client) PITR(ctx context.Context, table string) (bool, error) {
	out, err := c.call(ctx, "dynamodb", "describe-continuous-backups", "--table-name", table)
	if err != nil {
		return false, err
	}
	var r struct {
		ContinuousBackupsDescription struct {
			PointInTimeRecoveryDescription struct{ PointInTimeRecoveryStatus string }
		}
	}
	if err := json.Unmarshal(out, &r); err != nil {
		return false, fmt.Errorf("read backups setting of %s: %w", table, err)
	}
	return strings.EqualFold(r.ContinuousBackupsDescription.PointInTimeRecoveryDescription.PointInTimeRecoveryStatus, "ENABLED"), nil
}

// EnablePITR turns point-in-time recovery on.
func (c Client) EnablePITR(ctx context.Context, table string) error {
	_, err := c.call(ctx, "dynamodb", "update-continuous-backups", "--table-name", table,
		"--point-in-time-recovery-specification", "PointInTimeRecoveryEnabled=true")
	return err
}

// EnableStream turns the table's stream on with viewType.
func (c Client) EnableStream(ctx context.Context, table, viewType string) error {
	_, err := c.call(ctx, "dynamodb", "update-table", "--table-name", table,
		"--stream-specification", "StreamEnabled=true,StreamViewType="+viewType)
	return err
}

// EnableDeletionProtection turns deletion protection on.
func (c Client) EnableDeletionProtection(ctx context.Context, table string) error {
	_, err := c.call(ctx, "dynamodb", "update-table", "--table-name", table, "--deletion-protection-enabled")
	return err
}
