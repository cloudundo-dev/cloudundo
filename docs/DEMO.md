# Recording the 30-second demo

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

## Afterwards

Empty and delete the bucket in the S3 console.
