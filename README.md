# cloudundo

**Your AI agent can delete anything. You can always get it back.**

<!-- Demo GIF: docs/demo.gif (recorded before launch) -->

cloudundo sits in front of the AWS, Terraform and CloudFormation commands your coding agent runs (Claude Code, Cursor, Codex). Before a command deletes or overwrites data in S3, DynamoDB or RDS, cloudundo makes that change recoverable. If something goes wrong, `cloudundo undo` brings the data back. For ElastiCache (Redis, Valkey) it makes sure a snapshot is kept, or stops the delete.

> **Status: early, v0.1.** Every case is tested against a simulated AWS. Run by hand on real AWS, and undone: S3 object delete, overwrite and bucket delete; DynamoDB item and table delete; an RDS delete through the AWS CLI; a `terraform destroy` of a database, a bucket and a table; a CloudFormation deploy that drops a table and a stack delete; and `cleanup`. The agent hooks follow each agent's documentation and are being tried inside Claude Code, Cursor and Codex ([checklist](docs/TRY-THE-HOOKS.md)). Try it on a test account before production data.

```text
you:    "clean up the old test data"
agent:  aws s3 rm s3://acme-uploads/customers/ --recursive
        cloudundo: protected #1 — Undo with: cloudundo undo 1
you:    cloudundo undo 1 --yes
        Restored (4 steps).
```

## Why

AI coding agents have deleted real data. In March 2026 Claude Code ran `terraform destroy` on the wrong state and wiped a production database with 2.5 years of records, together with the snapshots meant to be its backup; AWS support restored it about a day later ([Tom's Hardware](https://www.tomshardware.com/tech-industry/artificial-intelligence/claude-code-deletes-developers-production-setup-including-its-database-and-snapshots-2-5-years-of-records-were-nuked-in-an-instant)). That is the case cloudundo is built for: it reads the Terraform plan first, snapshots the database, and refuses commands that delete snapshots.

It is not only production. Staging and dev accounts hold data people need too: a shared staging database, test fixtures that took a week to build, uploaded files. Agents work there all day, often with broad permissions, because it is "only staging".

Most guardrails try to *block* risky commands, which slows agents down until people turn the guardrails off. cloudundo takes the other route: let the agent work, and make sure a delete can be undone.

### How it compares

| | Stops the delete | Gets the data back | The agent keeps working |
| --- | --- | --- | --- |
| IAM deny policies | yes, for what you denied | no | no, its calls fail |
| Backups, point-in-time recovery | no | yes, restored by hand, if they were turned on and not deleted too | yes |
| The agent's own permission rules and deny lists | yes, for the commands listed | no | no, the command is refused or waits for you |
| **cloudundo** | only when it cannot make the data recoverable | yes: `cloudundo undo` | yes |

They work together. Keep your backups and IAM policies; cloudundo covers the moment between an agent's mistake and the restore, and its `policy` command adds the IAM rules that keep its own backups safe. What it changes in your account, and what it never does, is in [SECURITY.md](SECURITY.md).

## Set up

Two steps, whatever agent you use.

**Step 1: install cloudundo** on the machine where your agent runs commands (Linux, macOS, or WSL on Windows). The script checks the download's SHA-256 before installing it to `~/.local/bin`:

```sh
curl -fsSL https://raw.githubusercontent.com/OWNER/cloudundo/main/install.sh | sh
```

Or download a binary from the releases page, or build it: `go build ./cmd/cloudundo`.

**Step 2: connect it to your agent**, so the agent asks cloudundo before every command:

| Agent | Step 2 |
|---|---|
| Claude Code | `cloudundo install claude-code --write`<br>or the plugin: `/plugin marketplace add OWNER/cloudundo` then `/plugin install cloudundo@cloudundo` |
| Cursor | `cloudundo install cursor --write`, then restart Cursor |
| Codex | `cloudundo install codex --write`, then run `/hooks` in Codex to trust it |
| Your own agent | Add five lines to your code ([below](#your-own-agent)) |

For Claude Code, use the command or the plugin, not both, or every command is checked twice.

`install` keeps your other settings and saves the old file next to it (`*.before-cloudundo`). Without `--write` it only prints what it would add.

That's it. Commands that do not touch AWS data pass straight through. The hook never approves a command for you; your agent's own permission settings still apply.

### Your own agent

If you built your own agent and it runs shell commands, ask cloudundo before each one. Run it in the same folder and environment the command will use. Exit code `0` means the data is protected and the command can run. Anything else means do not run it.

```python
import subprocess

def run_agent_command(command: str, cwd: str) -> str:
    guard = subprocess.run(["cloudundo", "protect", "--", command],
                           cwd=cwd, capture_output=True, text=True)
    if guard.returncode != 0:
        return "Not run. cloudundo said:\n" + guard.stderr
    result = subprocess.run(command, shell=True, cwd=cwd, capture_output=True, text=True)
    return guard.stderr + result.stdout + result.stderr
```

Return cloudundo's message to the model either way, so it knows what was protected or why the command was stopped. `cloudundo protect --json` gives the same result as JSON.

If your agent calls AWS from code (boto3, the AWS SDK) instead of running commands, cloudundo cannot see those calls. See the limits below.

## How it works

The hook calls the same logic as `cloudundo protect`, which you can also run yourself:

```sh
cloudundo protect -- 'aws s3 rm s3://uploads --recursive'
# cloudundo: protected #1
#   + turned on versioning for s3://uploads (it can be suspended later, not removed)
#   + old versions in s3://uploads now expire after 7 days
#   note: Undo works on s3://uploads for 7 days.
#   Undo with: cloudundo undo 1
```

Exit code 0 means the command is safe to run. Exit code 2 means it must not run: cloudundo could not protect it, and says why.

| The command would... | Before it runs, cloudundo... | `undo` then... |
| --- | --- | --- |
| delete or overwrite objects | turns on bucket versioning if it is off | removes the delete markers, or copies the earlier version back |
| delete a whole bucket (`rb --force`) | copies it (up to 5 GB) to a private backup bucket and checks the copy | recreates the bucket and copies the objects back |
| delete a DynamoDB table | makes an on-demand backup, waits until it is ready, and records the table's settings | restores the table under the same name, then puts back its TTL, tags, stream, point-in-time recovery and deletion protection |
| delete or change DynamoDB items (`delete-item`, `put-item`, `update-item`, `batch-write-item`, up to 100 items) | reads each item and saves it | saves the items' current values, then puts the old ones back |
| delete an RDS database or Aurora cluster | uses the final snapshot the command asks for, or takes a snapshot itself and waits until it is ready; records size, network, security groups, parameter groups and tags | restores it under the same name with those settings, then sets backup retention and re-adds a cluster's instances |
| delete an ElastiCache cluster, replication group or serverless cache | lets it run if AWS keeps a final snapshot (or it is Memcached, or a replica); otherwise blocks it and says to add `--final-snapshot-identifier` | – (create a new cache from the snapshot) |
| `redis-cli FLUSHALL` / `FLUSHDB` | blocks it: nothing outside the server can bring that data back | |
| `terraform destroy` or `terraform apply` (or `tofu`) | asks Terraform for the plan first (`plan -lock=false` into a temporary file, or `show` of a saved plan; it never applies anything), then protects every database, bucket, object, table and cache the plan deletes or replaces, as above | restores each of them as above |
| `aws cloudformation delete-stack`, `execute-change-set` or `deploy` | reads the stack's resources, or the change set (for `deploy`, one made with `--no-execute-changeset` and deleted again; nothing is deployed), then protects every bucket, table, database and cache it deletes or replaces, as above. Resources the template keeps (`DeletionPolicy: Retain`) are skipped | restores each of them as above |
| `cdk destroy`, `sam delete`, `pulumi destroy`, `terragrunt destroy`, `aws cloudformation update-stack` | blocks it, and says which protected command to use instead | |
| target something it cannot read (`$VAR`, `xargs`, `file://`) | blocks it | |
| suspend versioning, change lifecycle rules, delete a specific object version (`--version-id`), or touch the backup bucket | blocks it: deleting a version is permanent | |
| run PartiQL or transactions, turn off deletion protection or point-in-time recovery, turn on TTL, or delete a backup | blocks it | |
| delete an RDS or cache snapshot or automated backups, turn off RDS deletion protection, set backup retention to 0 | blocks it | |

```sh
cloudundo undo 1        # shows what it would restore; changes nothing
cloudundo undo 1 --yes  # restores it
cloudundo log           # everything cloudundo saw
cloudundo cleanup       # the backups it kept (see below)
```

**How long can you undo?**

| What cloudundo kept | Kept for | Where |
| --- | --- | --- |
| Old versions of S3 objects | 7 days (`CLOUDUNDO_RETENTION_DAYS`) on buckets cloudundo versioned; as long as your own rules keep them otherwise | in the bucket |
| Copy of a deleted bucket | until you delete it | `cloudundo-backup-<account>-<region>` bucket |
| DynamoDB table backup | until you delete it | DynamoDB backups |
| Saved DynamoDB items | until you delete them | on this computer, in `~/.cloudundo` |
| RDS snapshot | until you delete it | RDS snapshots |

Everything kept in AWS is billed for storage like any backup, and stays until you remove it. `cloudundo cleanup` lists them and deletes the ones you choose:

```sh
cloudundo cleanup                          # list every backup cloudundo kept; changes nothing
cloudundo cleanup --older-than 30d --yes   # delete the ones kept longer than 30 days
cloudundo cleanup 7 --yes                  # or those of entry #7 (--all for every one)
```

After that, `undo` of those entries says the backup is gone instead of restoring it. The agent hooks block `cleanup --yes`: only you decide which undos to give up. If you applied `cloudundo policy` to your own user, run cleanup with credentials it does not cover.

`~/.cloudundo` also holds the journal that `undo` reads, so if that folder is lost, the backups in AWS are still there, but you restore them yourself.

Things to know:

- **Undo never deletes data.** In S3 it only removes delete markers or copies an older version on top. In DynamoDB it saves the current value of an item before putting the old one back, and leaves items the command created. So an undo can be undone too.
- **Versioning is permanent.** Once on, it can be suspended but not removed.
- **A bucket the command also deletes is copied, not versioned.** `aws s3 rm s3://b --recursive && aws s3 rb s3://b` would fail on a versioned bucket (it is never empty), so cloudundo copies the bucket first and leaves versioning alone.
- **The 7-day expiry rule** is added only to buckets that were never versioned, so old versions you chose to keep are never expired. Existing lifecycle rules are kept as they are.
- **Restored buckets come back private**, with their objects but not their old policy, website, CORS or tags.
- **A database snapshot can take a while.** cloudundo waits up to 7 minutes; for a bigger database it blocks the delete and asks to run it again later, and the retry uses the same snapshot. Data written between the snapshot and the delete is not in it. Restoring takes minutes too: run the same `undo --yes` again to finish.
- **Restoring a DynamoDB table takes minutes.** The first `undo --yes` starts it; run the same command again once it is done to put the settings back. Auto scaling, alarms and IAM policies are not restored. On-demand backups are kept until you delete them, and AWS bills for their storage.
- **A slow or broken network blocks the command instead of hanging.** Each AWS call gets 2 minutes (a bucket copy gets 9), and the whole protection at most 9 minutes, under the agents' 10-minute hook limit. cloudundo says which call it is waiting for.
- cloudundo calls your own `aws` CLI with the same profile, region and environment as the agent's command, in any AWS region. Inline access keys are used but never written to disk.
- `cloudundo undo` uses the profile and region the agent used, even from a terminal set up differently. If your current credentials belong to another AWS account, it stops and names both accounts instead of restoring into the wrong one.
- **Every problem cloudundo can see blocks the command**: bad hook input, an AWS error, a full disk, its own bugs. If the hook program cannot start or times out, Claude Code (v2.1.295+, `onFailure: block`) and Cursor (`failClosed: true`) block too. Codex has no such setting, so there a missing or crashed cloudundo lets commands through.
- Undo puts each key back to how it was just before the command, using your computer's clock. If an app writes to the same keys after the command, undo will roll those writes back too, which is why it shows a preview first.

## What cloudundo does not protect against

Plainly, so you can decide where it fits:

- **cloudundo is a seatbelt for mistakes, not a lock against a hostile agent.** The agent still holds your AWS keys. cloudundo makes the deletes it sees recoverable; it does not take the keys away.
- **It sees command lines, not what programs do.** A `./deploy.sh`, a Python script or an SDK call (such as boto3) that deletes data is not seen by the hook. The policy below closes part of this gap.
- **Data deleted from inside a database is not seen.** `DROP TABLE` or `DELETE FROM` through `psql` or `mysql` goes straight to the database, not through the AWS CLI. Use your database's own backups (RDS automated backups, point-in-time recovery) for that.
- **S3, DynamoDB, RDS and ElastiCache**, through the AWS CLI, Terraform or CloudFormation. EBS, EFS, other clouds: not yet. Terraform restores the data, not the infrastructure around it (networks, load balancers, IAM): re-apply your Terraform for those.
- **A plan that deletes data cloudundo cannot back up yet** (EBS volumes, EFS, Redshift, DocumentDB, OpenSearch, KMS keys and similar) is blocked, with the resource named, instead of running unprotected.
- **Other deploy tools are only partly seen.** Their destroy commands (`cdk destroy`, `sam delete`, `pulumi destroy`, `terragrunt destroy`) are blocked, but `cdk deploy`, `sam deploy`, `pulumi up` and `terragrunt apply` can also replace or remove resources, and cloudundo does not see what they do. Neither does it see `make destroy` or a script that runs Terraform inside.
- **CloudFormation templates in YAML**: cloudundo cannot read their `DeletionPolicy`, so it backs up every data resource in the stack, including ones the template keeps. (CDK and JSON templates are read.)
- **Terraform commands take longer**: cloudundo makes a plan before the agent's apply or destroy makes its own, so expect up to twice the planning time.
- **Terraform needs to plan locally.** Remote runs on HCP Terraform / Terraform Cloud cannot save a plan, so cloudundo blocks those. A provider that uses a different AWS account than your current credentials is blocked too, with the account named.
- **Platforms outside AWS** (Supabase, Railway, Vercel, Replit) are not covered.
- **Big buckets:** a bucket delete is protected by copying it, up to 5 GB by default.

### Close the gap with an IAM policy

`cloudundo policy` prints an IAM policy for the **agent's** AWS user. Every statement is a Deny, so it only takes permissions away, and none of them blocks what cloudundo itself does. With it, AWS itself refuses, whatever tool the agent uses, to:

- delete or change cloudundo's backup bucket (`KeepCloudundoBackups`),
- delete DynamoDB backups (`KeepDynamoDBBackups`) or cloudundo's RDS snapshots (`KeepCloudundoDBSnapshots`),
- with `--keep-versions`, remove any old S3 object version, so deletes in versioned buckets can always be undone (`KeepS3VersionHistory`). Undo still works: it copies the old version back instead of removing delete markers.

```sh
cloudundo policy --account 123456789012 --region eu-west-1 --keep-versions > cloudundo-policy.json
aws iam put-user-policy --user-name <agent-user> --policy-name cloudundo-guard \
    --policy-document file://cloudundo-policy.json
```

Run those with your own admin credentials. The command also prints how to create the backup bucket first. Give the agent its own IAM user: if you share one with it, the policy limits you too (cleaning up old backups then needs other credentials). The policy still does not stop an agent from suspending versioning on your buckets, changing their lifecycle rules, or deleting data in buckets that were never versioned, because cloudundo needs those same permissions to protect you.

### Commands it understands

`cloudundo check -- '<command>'` shows what a command would change, without touching AWS. It reads shell lines the way your shell would: `&&` chains, `bash -c "..."`, `sudo`/`env` wrappers, `export`, loops and `$(...)`.

| Command | Seen as |
| --- | --- |
| `aws s3 rm`, `s3api delete-object`, `delete-objects` | delete objects |
| `aws s3 rb`, `s3api delete-bucket` | delete bucket |
| `aws s3 mv` | delete at the source, overwrite at the destination |
| `aws s3 cp` / `sync` to S3, `s3api put-object`, `copy-object` | overwrite (and delete with `sync --delete`) |
| suspending versioning, lifecycle rules | weakens protection |
| `aws dynamodb delete-table` | delete table |
| `delete-item`, `put-item`, `update-item`, `batch-write-item` | delete or overwrite items |
| `execute-statement`, `transact-write-items` and other PartiQL or transactions | not readable, blocked |
| `aws rds delete-db-instance`, `delete-db-cluster` | delete database |
| `aws elasticache delete-cache-cluster`, `delete-replication-group`, `delete-serverless-cache` | delete cache |
| `redis-cli` / `valkey-cli` with `FLUSHALL` or `FLUSHDB` | erase cache, blocked |
| `terraform destroy`, `terraform apply [plan]`, `tofu …`, after any `cd` or `-chdir` | read from its plan |
| `aws cloudformation delete-stack`, `execute-change-set`, `deploy` | read from the stack or its change set |
| `cdk destroy`, `sam delete`, `pulumi destroy`/`down`, `terragrunt … destroy`, `aws cloudformation update-stack`, `delete-stack-instances` | not readable, blocked |
| `cloudundo cleanup --yes` | deletes cloudundo's backups, blocked for agents |
| `aws backup delete-recovery-point`, `delete-backup-vault`, `update-recovery-point-lifecycle`, `delete-backup-plan` | weakens protection (AWS Backup), blocked |
| `update-table` turning off deletion protection, `update-continuous-backups` turning off recovery, `update-time-to-live` turning on TTL, `delete-backup` | weakens protection |

### Settings

| Variable | Default | Meaning |
| --- | --- | --- |
| `CLOUDUNDO_HOME` | `~/.cloudundo` | where the journal lives |
| `CLOUDUNDO_RETENTION_DAYS` | 7 | undo window on buckets cloudundo versions |
| `CLOUDUNDO_MAX_COPY_GB` | 5 | largest bucket copied before a bucket delete |
| `CLOUDUNDO_AWS_BIN` | `aws` | which aws CLI to run |

## Roadmap

1. ~~S3: versioning before object deletes, bucket copies before bucket deletes, `undo`.~~ Done.
2. ~~DynamoDB: on-demand backup before table deletes, saved items before item deletes, `undo`.~~ Done.
3. ~~Hooks for Claude Code, Cursor and Codex, failing closed.~~ Done; being tried inside each agent.
4. ~~Real-AWS run of every case.~~ Done by hand; an automated real-AWS test suite is next.
5. ~~RDS snapshots before deletes, ElastiCache snapshot rules, Terraform plans.~~ Done and run on real AWS.
6. ~~CloudFormation stacks and change sets.~~ Done and run on real AWS.
7. ~~`cloudundo cleanup` for old backups.~~ Done.
8. CDK, SAM, Pulumi and Terragrunt plans; backups kept outside the user's account; agents without long-lived keys (for teams); other clouds.

## Development

```sh
make build     # ./cloudundo
make check     # vet, gofmt, tests with -race, short fuzzing
make release   # dist/: binaries for Linux, macOS, Windows + checksums.txt
```

Recording a demo: [docs/DEMO.md](docs/DEMO.md).

No dependencies outside the Go standard library.

## License

Apache 2.0. See [LICENSE](LICENSE).
