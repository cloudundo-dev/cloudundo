// Package policy builds the IAM policy that keeps an agent's credentials
// from destroying what cloudundo needs for undo, even through scripts,
// SDKs or Terraform that the hook never sees.
//
// Every statement is a Deny, so attaching it can only take permissions
// away. It never denies anything cloudundo itself does while protecting a
// command (turning versioning on for a user bucket, adding the expiry
// rule, creating and writing the backup bucket, making table backups),
// because cloudundo runs with the agent's own credentials.
package policy

import (
	"encoding/json"
	"fmt"
	"regexp"
)

// BackupBucketPrefix matches protect.BackupBucketPrefix.
const BackupBucketPrefix = "cloudundo-backup-"

// Options narrow the policy to one account and region.
type Options struct {
	Account string // 12 digits, or "" for any
	Region  string // e.g. eu-west-1, or "" for any
	// KeepVersions denies removing any S3 object version in the account,
	// so deletes in versioned buckets can always be undone.
	KeepVersions bool
}

// Statement is one IAM policy statement.
type Statement struct {
	Sid      string   `json:"Sid"`
	Effect   string   `json:"Effect"`
	Action   []string `json:"Action"`
	Resource []string `json:"Resource"`
}

// Document is an IAM policy document.
type Document struct {
	Version   string      `json:"Version"`
	Statement []Statement `json:"Statement"`
}

var (
	accountRe = regexp.MustCompile(`^[0-9]{12}$`)
	regionRe  = regexp.MustCompile(`^[a-z]{2}(-[a-z]+)+-[0-9]$`)
)

// Validate checks the options.
func (o Options) Validate() error {
	if o.Account != "" && !accountRe.MatchString(o.Account) {
		return fmt.Errorf("--account %q is not a 12-digit AWS account id", o.Account)
	}
	if o.Region != "" && !regionRe.MatchString(o.Region) {
		return fmt.Errorf("--region %q is not an AWS region such as eu-west-1", o.Region)
	}
	return nil
}

// BackupBucket is the backup bucket name, with * for unknown parts.
func (o Options) BackupBucket() string {
	acct, region := o.Account, o.Region
	if acct == "" {
		acct = "*"
	}
	if region == "" {
		region = "*"
	}
	name := BackupBucketPrefix + acct + "-" + region
	if acct == "*" && region == "*" {
		name = BackupBucketPrefix + "*"
	}
	return name
}

// Build returns the policy for o.
func Build(o Options) Document {
	bucket := o.BackupBucket()
	region, acct := o.Region, o.Account
	if region == "" {
		region = "*"
	}
	if acct == "" {
		acct = "*"
	}
	d := Document{Version: "2012-10-17", Statement: []Statement{
		{
			Sid:    "KeepCloudundoBackups",
			Effect: "Deny",
			Action: []string{
				"s3:DeleteBucket",
				"s3:DeleteObject",
				"s3:DeleteObjectVersion",
				"s3:PutLifecycleConfiguration",
				"s3:PutBucketVersioning",
				"s3:PutBucketPolicy",
				"s3:DeleteBucketPolicy",
			},
			Resource: []string{
				"arn:aws:s3:::" + bucket,
				"arn:aws:s3:::" + bucket + "/*",
			},
		},
		{
			Sid:    "KeepCloudundoDBSnapshots",
			Effect: "Deny",
			Action: []string{"rds:DeleteDBSnapshot", "rds:DeleteDBClusterSnapshot"},
			Resource: []string{
				fmt.Sprintf("arn:aws:rds:%s:%s:snapshot:cloudundo-*", region, acct),
				fmt.Sprintf("arn:aws:rds:%s:%s:cluster-snapshot:cloudundo-*", region, acct),
			},
		},
		{
			Sid:      "KeepDynamoDBBackups",
			Effect:   "Deny",
			Action:   []string{"dynamodb:DeleteBackup"},
			Resource: []string{fmt.Sprintf("arn:aws:dynamodb:%s:%s:table/*/backup/*", region, acct)},
		},
	}}
	if o.KeepVersions {
		d.Statement = append(d.Statement, Statement{
			Sid:      "KeepS3VersionHistory",
			Effect:   "Deny",
			Action:   []string{"s3:DeleteObjectVersion"},
			Resource: []string{"arn:aws:s3:::*/*"},
		})
	}
	return d
}

// JSON renders the document for `aws iam put-user-policy`.
func (d Document) JSON() string {
	b, _ := json.MarshalIndent(d, "", "  ")
	return string(b) + "\n"
}
