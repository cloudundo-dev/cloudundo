# cloudundo

**Your AI agent can delete anything. You can always get it back.**

![Claude Code deletes four files from S3; cloudundo saved them first, and cloudundo undo brings them back](assets/demo.gif)

cloudundo sits in front of the AWS, Terraform and CloudFormation commands your coding agent runs (Claude Code, Cursor, Codex). Before a command deletes or overwrites data in S3, DynamoDB or RDS, cloudundo makes that change recoverable. If something goes wrong, `cloudundo undo` brings the data back. For ElastiCache (Redis, Valkey) it makes sure a snapshot is kept, or stops the delete.

> **Status: early, v0.1.** Tested on real AWS (S3, DynamoDB, RDS, Terraform, CloudFormation) and inside Claude Code and Cursor. Not tried inside Codex yet. Try it on a test account before production data.

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

About two minutes, on the computer where your agent runs its commands (Linux, macOS, or WSL on Windows).

**1. Install cloudundo**

```sh
curl -fsSL https://raw.githubusercontent.com/cloudundo-dev/cloudundo/main/install.sh | sh
```

This downloads the cloudundo program for your computer, checks that the download is the real one (SHA-256), and puts it in `~/.local/bin`.

Want to see what it does first? `cloudundo check` reads a command and says what it would change, without touching AWS:

```sh
cloudundo check -- 'aws s3 rm s3://uploads --recursive'
cloudundo check -- 'aws s3 ls s3://uploads'
cloudundo check -- 'aws s3 rm s3://$BUCKET --recursive'
```

The first prints `DELETE     s3://uploads/*`: cloudundo would make it recoverable before it runs. The second prints `No data changes found.`: it runs as usual. The third prints `DELETE     s3://$BUCKET/*  (target not fully known)`: cloudundo would block it, because it cannot tell which bucket it would empty.

That is only a preview. The protection and `undo` start in step 2, once cloudundo is connected to your agent.

**2. Connect it to your agents**

```sh
cloudundo install --write
```

This finds Claude Code, Cursor and Codex on your computer and tells each of them to ask cloudundo before it runs a command. Your other settings stay as they are, and the old settings file is saved next to the new one (`*.before-cloudundo`). Run `cloudundo install` without `--write` first if you want to see what it would change.

**3. Restart your agent**

- **Claude Code:** start a new session. Type `/hooks` to see cloudundo listed.
- **Cursor:** quit it and open it again.
- **Codex:** run `/hooks` once and trust the cloudundo hook.

**4. Check that it works**, without touching any data. Ask your agent:

> This is a test of my cloudundo safety hook. Please run exactly this command so the hook can check it: `aws s3 rm s3://$NO_SUCH_BUCKET --recursive`

cloudundo cannot tell which bucket that is, so it blocks the command, and the agent tells you so. (Some agents refuse it on their own first; telling them it is a test helps.) Then run `cloudundo log` to see the blocked command and which agent sent it.

From now on, commands that do not touch AWS data run as usual, and your agent's own permission prompts stay as they are. When a command would delete data, cloudundo makes it recoverable first. `cloudundo log` lists each command with its number, and `cloudundo undo <number>` brings the data back (Claude Code also shows that command in the chat).

<details>
<summary><b>Windows, PATH problems, one agent at a time, the Claude Code plugin</b></summary>

- **Windows:** install and run it inside WSL (Ubuntu), where your agent runs its commands. With Cursor, open your project with **Connect to WSL**.
- **`cloudundo: command not found`:** `~/.local/bin` is not on your PATH yet. Run `echo 'export PATH="$HOME/.local/bin:$PATH"' >> ~/.bashrc && source ~/.bashrc` (or `~/.zshrc` on macOS).
- **One agent at a time:** `cloudundo install claude-code --write` (or `cursor`, `codex`). Without `--write`, `install` only shows what it would add. Each changed file is saved next to itself as `*.before-cloudundo`.
- **Claude Code plugin**, instead of `install`: `/plugin marketplace add cloudundo-dev/cloudundo`, then `/plugin install cloudundo@cloudundo`. Use one or the other, not both, or every command is checked twice.
- **No install script:** download a binary from the releases page, or build it with `go build ./cmd/cloudundo`.

</details>

### Your own agent

If you built your own agent and it runs shell commands, ask cloudundo before each one. Run it in the same folder and environment the command will use. Exit code `0` means the data is protected and the command can run. Anything else means do not run it.

```python
import subprocess

def run_agent_command(command: str, cwd: str) -> str:
    guard = subprocess.run(["cloudundo", "protect", "--", command],
                           cwd=cwd, capture_output=True, text=True)
    note = guard.stdout + guard.stderr
    if guard.returncode != 0:
        return "Not run. cloudundo said:\n" + note
    result = subprocess.run(command, shell=True, cwd=cwd, capture_output=True, text=True)
    return note + result.stdout + result.stderr
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
- **A command that is not written out plainly is not seen yet.** One hidden inside `eval`, piped into a shell (`echo "aws s3 rm ..." | bash`), built from a variable (`$CMD s3 rm ...`) or run in a container (`docker run amazon/aws-cli ...`) passes without protection. `bash -c "..."`, `sudo`, `env` and `xargs` are seen.
- **Data deleted from inside a database is not seen.** `DROP TABLE` or `DELETE FROM` through `psql` or `mysql` goes straight to the database, not through the AWS CLI. Use your database's own backups (RDS automated backups, point-in-time recovery) for that.
- **S3, DynamoDB, RDS and ElastiCache**, through the AWS CLI, Terraform or CloudFormation. EBS, EFS, other clouds: not yet. Terraform restores the data, not the infrastructure around it (networks, load balancers, IAM): re-apply your Terraform for those.
- **A plan that deletes data cloudundo cannot back up yet** (EBS volumes, EFS, Redshift, DocumentDB, OpenSearch, KMS keys and similar) is blocked, with the resource named, instead of running unprotected.
- **Other deploy tools are only partly seen.** Their destroy commands (`cdk destroy`, `sam delete`, `pulumi destroy`, `terragrunt destroy`) are blocked, but `cdk deploy`, `sam deploy`, `pulumi up` and `terragrunt apply` can also replace or remove resources, and cloudundo does not see what they do. Neither does it see `make destroy` or a script that runs Terraform inside.
- **CloudFormation templates in YAML**: cloudundo cannot read their `DeletionPolicy`, so it backs up every data resource in the stack, including ones the template keeps. (CDK and JSON templates are read.)
- **Terraform commands take longer**: cloudundo makes a plan before the agent's apply or destroy makes its own, so expect up to twice the planning time.
- **Terraform needs to plan locally.** Remote runs on HCP Terraform / Terraform Cloud cannot save a plan, so cloudundo blocks those. A provider that uses a different AWS account than your current credentials is blocked too, with the account named. cloudundo reads the plan in the folder the agent says the command runs in (in Cursor, the open project). If the agent gives no folder, or the plan finds no Terraform state there, a `terraform destroy` is blocked rather than let through with nothing saved; `terraform -chdir=/full/path` works anywhere.
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

Done:

- S3: versioning before object deletes, bucket copies before bucket deletes
- DynamoDB: a backup before table deletes, saved items before item deletes
- RDS snapshots before deletes, ElastiCache snapshot rules
- Terraform and CloudFormation: what a plan or change set deletes is protected first
- Hooks for Claude Code, Cursor and Codex that block when anything goes wrong
- `cloudundo cleanup` for old backups
- Every case run by hand on real AWS, and inside Claude Code and Cursor

Next:

- An automated test suite on real AWS
- CDK, SAM, Pulumi and Terragrunt
- Backups kept outside your own account
- Agents that hold no long-lived AWS keys (for teams)
- Other clouds

What would you want next? [Tell us](https://github.com/cloudundo-dev/cloudundo/issues/new?template=idea.yml): a real case of what your agent did, or what you are worried it might do, helps most.

If cloudundo saved you once, a ⭐ on GitHub helps others find it.

## Development

```sh
make build     # ./cloudundo
make check     # vet, gofmt, tests with -race, short fuzzing
make release   # dist/: binaries for Linux, macOS, Windows + checksums.txt
```

No dependencies outside the Go standard library.

## License

Copyright 2026 Youssef Khalil. Apache 2.0. See [LICENSE](LICENSE).
