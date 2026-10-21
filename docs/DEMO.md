# Recording the demo

The story in one line: **an AI agent deletes your files, one command brings them back.**
Record on a test AWS account, with a big terminal font and nothing secret on screen
(no account numbers: run `export AWS_PAGER=` and avoid `sts get-caller-identity`).

## Set up (not recorded)

```sh
B=acme-uploads-$RANDOM
aws s3 mb s3://$B >/dev/null
for f in invoice-001.pdf invoice-002.pdf contract.pdf logo.png; do
  echo "important: $f" > $f && aws s3 cp $f s3://$B/customers/$f >/dev/null
done
rm -f invoice-*.pdf contract.pdf logo.png
cloudundo install claude-code --write
clear
```

## Record (about 30 seconds)

1. **(0–5 s)** Show the files:
   `aws s3 ls s3://$B/customers/` → four files.
2. **(5–15 s)** In Claude Code, ask:
   `Clean up the old test data: run aws s3 rm s3://<bucket>/customers/ --recursive`
   Approve it. Claude deletes the files; the cloudundo note appears.
3. **(15–20 s)** Show the damage:
   `aws s3 ls s3://$B/customers/` → nothing.
4. **(20–30 s)** Undo:
   `cloudundo log` → the entry.
   `cloudundo undo 1 --yes` → "Restored (4 steps)."
   `aws s3 ls s3://$B/customers/` → all four files are back.

End card: **cloudundo — your agent can delete anything; you can always get it back.**

## Without an agent (a terminal-only take)

If Claude Code is not set up, the same story runs with the hook's own logic:

```sh
cloudundo protect -- "aws s3 rm s3://$B/customers/ --recursive" && aws s3 rm s3://$B/customers/ --recursive
aws s3 ls s3://$B/customers/
cloudundo undo 1 --yes
aws s3 ls s3://$B/customers/
```

## The Terraform take (about 60 seconds after editing)

This one tells the story people already know: an agent runs `terraform destroy` and the database is gone. Record it **during Test 7** in `docs/REAL-AWS-TEST.md`, so the AWS setup and the waiting happen once. Cut the waiting out when editing.

Set up: everything in Test 7 up to the `put-item` line, then `clear`.

1. **(0–10 s)** What exists. One line per thing, nothing secret on screen:
   `aws rds describe-db-instances --db-instance-identifier cu-tf-db --query 'DBInstances[0].DBInstanceStatus'` → `"available"`
   `aws s3 cp s3://$BUCKET/hello.txt -` → `hello`
2. **(10–25 s)** The agent. In Claude Code (hook or plugin installed), ask:
   `This test stack is finished. Run terraform destroy -auto-approve in this folder.`
   Approve it. The cloudundo note appears: a snapshot of `cu-tf-db`, a copy of the bucket, a backup of the table, and "Undo with: cloudundo undo N".
   *(Cut: the 5–10 minutes of destroying. Keep the last line, "Destroy complete!")*
3. **(25–35 s)** The damage: the same two lines as step 1 now fail (`DBInstanceNotFound`, `NoSuchBucket`).
4. **(35–55 s)** Undo:
   `cloudundo undo N --yes` → "Started … run again in a few minutes".
   *(Cut: the wait. A caption such as "15 minutes later" is honest.)*
   `cloudundo undo N --yes` → "Restored".
5. **(55–60 s)** The two lines from step 1 again: `"available"` and `hello`.

End card: **cloudundo — your agent can delete anything; you can always get it back.**

If Claude Code is not part of the take, step 2 can be the terminal line from Test 7:
`cloudundo protect -- "terraform destroy -auto-approve" && terraform destroy -auto-approve`.

## Afterwards

Empty and delete the bucket in the S3 console. For the Terraform take, follow the clean-up at the end of Test 7.
