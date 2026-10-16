# cloudundo

**Your AI agent can delete anything. You can always get it back.**

cloudundo sits in front of the AWS commands your coding agent runs (Claude Code, Cursor, Codex). Before a command deletes or overwrites data in S3 or DynamoDB, cloudundo makes that change recoverable. If something goes wrong, `cloudundo undo` brings the data back.

> **Status: early development.** Protection and `undo` for S3 and DynamoDB work and are tested against a simulated AWS, but have not yet been run against real AWS. The agent hooks come next. Try it on a test account, not production data.

## Why

AI coding agents have deleted real data: a Codex session reduced a 3 TB S3 bucket to 34 KB, and a Cursor agent deleted a production database and its backups in nine seconds. Most guardrails try to *block* risky commands, which slows agents down until people turn the guardrails off. cloudundo takes the other route: let the agent work, and make sure every delete can be undone.

## How it works

Run `cloudundo protect` before an agent's command (the hooks for Claude Code, Cursor and Codex will do this for you):

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
| target something it cannot read (`$VAR`, `xargs`, `file://`) | blocks it | |
| suspend versioning, change lifecycle rules, or touch the backup bucket | blocks it | |
| run PartiQL or transactions, turn off deletion protection or point-in-time recovery, turn on TTL, or delete a backup | blocks it | |

```sh
cloudundo undo 1        # shows what it would restore; changes nothing
cloudundo undo 1 --yes  # restores it
cloudundo log           # everything cloudundo saw
```

Things to know:

- **Undo never deletes data.** In S3 it only removes delete markers or copies an older version on top. In DynamoDB it saves the current value of an item before putting the old one back, and leaves items the command created. So an undo can be undone too.
- **Versioning is permanent.** Once on, it can be suspended but not removed.
- **The 7-day expiry rule** is added only to buckets that were never versioned, so old versions you chose to keep are never expired. Existing lifecycle rules are kept as they are.
- **Restored buckets come back private**, with their objects but not their old policy, website, CORS or tags.
- **Restoring a DynamoDB table takes minutes.** The first `undo --yes` starts it; run the same command again once it is done to put the settings back. Auto scaling, alarms and IAM policies are not restored. On-demand backups are kept until you delete them, and AWS bills for their storage.
- cloudundo calls your own `aws` CLI with the same profile, region and environment as the agent's command. Inline access keys are used but never written to disk.
- Undo puts each key back to how it was just before the command, using your computer's clock. If an app writes to the same keys after the command, undo will roll those writes back too, which is why it shows a preview first.

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
3. Hooks for Claude Code, Cursor and Codex, failing closed.
4. Real-AWS tests for every case before the first release.

## Development

```sh
go build ./cmd/cloudundo
go vet ./...
go test -race ./...
go test -run=^$ -fuzz=FuzzAnalyze -fuzztime=60s ./internal/effects
```

No dependencies outside the Go standard library.

## License

Apache 2.0. See [LICENSE](LICENSE).
