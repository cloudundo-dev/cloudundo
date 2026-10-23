# Security

cloudundo sits between an AI agent and your AWS account, so you should know exactly what it does there. This page lists all of it.

## What it protects against, and what not

cloudundo is a seatbelt for **mistakes**: an agent that deletes data it should not have. It makes those deletes recoverable, or blocks them when it cannot.

It is **not** a lock against a hostile agent. The agent still holds your AWS keys, and it can reach AWS in ways cloudundo does not see (a script, an SDK call). [`cloudundo policy`](README.md#close-the-gap-with-an-iam-policy) prints an IAM policy that stops those keys from deleting cloudundo's backups whatever tool they use. For a hostile agent, take the keys away.

## Where it runs

- One program on your computer, with no dependencies outside the Go standard library.
- It reaches AWS only by running your own `aws` CLI, with the agent's profile and region. For Terraform it runs your own `terraform plan` (`-lock=false`, into a temporary file; it never applies anything).
- **No telemetry.** It sends nothing anywhere else: no servers, no accounts, no analytics.
- The agent hooks never approve a command for you; your agent's own permission prompts stay as they are. Every problem cloudundo can see blocks the command (fail closed), and it stops itself after 9 minutes, under the agents' 10-minute hook limit.

## What it changes in your AWS account

| What | When | Name |
| --- | --- | --- |
| Turns on bucket versioning (permanent; it can be suspended, not removed) | before objects in a bucket are deleted or overwritten | your bucket |
| Adds a lifecycle rule that expires old versions after 7 days | when it turned versioning on itself | `cloudundo-expire-old-versions` |
| Creates a private backup bucket (public access blocked, versioned) and copies buckets into it | before a bucket is deleted | `cloudundo-backup-<account>-<region>` |
| Creates DynamoDB on-demand backups | before a table is deleted | `cloudundo-<table>-<time>` |
| Creates RDS snapshots | before a database is deleted | `cloudundo-<database>-<time>` |
| Creates, reads and deletes a CloudFormation change set | before `aws cloudformation deploy` | made by `deploy --no-execute-changeset` |

It never deletes your data. `undo` removes S3 delete markers and copies earlier versions back; it saves the current value of a DynamoDB item before putting an old one back. The only command that deletes anything is `cloudundo cleanup --yes`, which deletes cloudundo's own backups when **you** run it; the agent hooks block it.

Backups are billed for storage like any other. `cloudundo cleanup` lists them.

## What it keeps on your computer

In `~/.cloudundo` (folder `0700`, files `0600`):

- `journal.jsonl`: every command it saw, the names of the resources, and the AWS profile and region names the agent used. **Never access keys**: keys given inline on a command line are used for that call and never written down.
- Saved DynamoDB items: the old values of items before an agent changed them. This is your data; treat the folder like the table.

## AWS permissions it uses

cloudundo uses the agent's credentials. These are the calls it makes, by service:

- **STS:** `GetCallerIdentity`
- **S3:** `HeadBucket`, `GetBucketLocation`, `GetBucketVersioning`, `PutBucketVersioning`, `GetLifecycleConfiguration`, `PutLifecycleConfiguration`, `ListBucket` / `ListBucketVersions`, `GetObject`, `PutObject` (copies), `DeleteObject` (delete markers on undo; backup copies on cleanup), `CreateBucket`, `PutPublicAccessBlock`
- **DynamoDB:** `DescribeTable`, `GetItem`, `PutItem`, `CreateBackup`, `DescribeBackup`, `DeleteBackup` (cleanup), `RestoreTableFromBackup`, `DescribeTimeToLive`, `UpdateTimeToLive`, `DescribeContinuousBackups`, `UpdateContinuousBackups`, `ListTagsOfResource`, `TagResource`, `UpdateTable`
- **RDS:** `DescribeDBInstances`, `DescribeDBClusters`, `CreateDBSnapshot`, `CreateDBClusterSnapshot`, `DescribeDBSnapshots`, `DescribeDBClusterSnapshots`, `DeleteDBSnapshot` / `DeleteDBClusterSnapshot` (cleanup), `RestoreDBInstanceFromDBSnapshot`, `RestoreDBClusterFromSnapshot`, `CreateDBInstance` (Aurora cluster members on undo), `ModifyDBInstance` / `ModifyDBCluster` (backup retention on undo), `AddTagsToResource`
- **ElastiCache:** `DescribeCacheClusters`, `DescribeServerlessCaches`
- **CloudFormation:** `DescribeStacks`, `ListStackResources`, `GetTemplate`, `DescribeChangeSet`, `CreateChangeSet` and `DeleteChangeSet` (for `deploy`)

If a call is denied, the command is blocked with the reason; nothing runs unprotected.

## Releases

Release binaries are built from this repository by GitHub Actions, with a `checksums.txt` (SHA-256). `install.sh` downloads the binary for your system and refuses to install it unless its checksum matches. You can also build it yourself: `go build ./cmd/cloudundo`.

## Reporting a vulnerability

Please report it privately through GitHub: **Security → Report a vulnerability** on this repository. Do not open a public issue for it.
