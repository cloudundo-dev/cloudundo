# cloudundo

**Your AI agent can delete anything. You can always get it back.**

cloudundo sits in front of the AWS commands your coding agent runs (Claude Code, Cursor, Codex). Before a command deletes or overwrites data in S3 or DynamoDB, cloudundo makes that change recoverable. If something goes wrong, `cloudundo undo` brings the data back.

> **Status: early development.** Today cloudundo reads commands and reports what they would change in S3. Protection (versioning, backups), `undo`, DynamoDB and the agent hooks are being built now. Do not rely on it to protect data yet.

## Why

AI coding agents have deleted real data: a Codex session reduced a 3 TB S3 bucket to 34 KB, and a Cursor agent deleted a production database and its backups in nine seconds. Most guardrails try to *block* risky commands, which slows agents down until people turn the guardrails off. cloudundo takes the other route: let the agent work, and make sure every delete can be undone.

## What it understands today

`cloudundo check` reads a shell command the way your shell would, including `&&` chains, `bash -c "..."`, `sudo`/`env` wrappers, loops and `$(...)`, and reports S3 effects:

| Command | Reported as |
| --- | --- |
| `aws s3 rm s3://b/k`, `--recursive` | delete objects |
| `aws s3 rb s3://b --force` | delete bucket with all objects |
| `aws s3 mv` | delete at the source, overwrite at the destination |
| `aws s3 cp` / `sync` to S3 | overwrite (and delete with `sync --delete`) |
| `aws s3api delete-object`, `delete-objects`, `delete-bucket` | delete |
| `aws s3api put-object`, `copy-object` | overwrite |
| suspending versioning, lifecycle rules | weakens protection |

When a target cannot be read from the command (a variable, `$(...)`, `xargs`, a `file://` payload), the effect is marked **uncertain** instead of guessed.

## Try it

```sh
go build ./cmd/cloudundo

./cloudundo check -- 'cd infra && aws s3 rb s3://user-data --force'
# DELETE BUCKET s3://user-data  (with all objects)

./cloudundo check --json -- 'aws s3 sync ./dist s3://site --delete'
./cloudundo check --record -- 'aws s3 rm s3://uploads/tmp/ --recursive'
./cloudundo log
```

The journal lives in `~/.cloudundo` (override with `CLOUDUNDO_HOME`).

## Roadmap

1. S3: turn on versioning before object deletes, copy small buckets before bucket deletes, `undo`.
2. DynamoDB: on-demand backup before table deletes, saved items before item deletes, `undo`.
3. Hooks for Claude Code, Cursor and Codex.
4. Real-AWS tests for every case before the first release.

## Development

```sh
go vet ./...
go test -race ./...
go test -run=^$ -fuzz=FuzzAnalyze -fuzztime=60s ./internal/s3effects
```

No dependencies outside the Go standard library.

## License

Apache 2.0. See [LICENSE](LICENSE).
