# cloudundo: first test on real AWS

Use your personal laptop and a NEW, separate AWS account. Never the work laptop or a work account.
On Windows, run everything inside WSL (Ubuntu). The commands below are for Mac / Linux / WSL.

## 0. Setup (once)

1. Create a new AWS account (personal email). Turn on MFA for the root user.
2. Billing → Budgets → create a $5 monthly budget with an email alert.
3. IAM → Users → Create user `cloudundo-test`. Attach `AmazonS3FullAccess` and `AmazonDynamoDBFullAccess`.
   Security credentials → Create access key → "Command Line Interface".
4. Install AWS CLI v2, then:

```sh
aws configure --profile cloudundo-test      # paste the key, region: us-east-1, output: json
export AWS_PROFILE=cloudundo-test
aws sts get-caller-identity                  # MUST show the NEW account number
```

5. Pick the file for your computer, rename it, and make it runnable:

```sh
# Mac with Apple chip: cloudundo-darwin-arm64   | Intel Mac: cloudundo-darwin-amd64 | Linux/WSL: cloudundo-linux-amd64
mv cloudundo-darwin-arm64 cloudundo
chmod +x cloudundo
xattr -d com.apple.quarantine cloudundo 2>/dev/null   # Mac only
./cloudundo version                                    # cloudundo 0.1.0-test
```

Every test below works the same way, like the agent hook will:
write the command to `cmd.txt`, let cloudundo protect it, and run it only if cloudundo says yes (`&&`).

## Test 1: delete a file, then undo

```sh
B=cloudundo-test-$RANDOM$RANDOM
aws s3 mb s3://$B
echo hello > a.txt && aws s3 cp a.txt s3://$B/a.txt

echo "aws s3 rm s3://$B/a.txt" > cmd.txt
./cloudundo protect -- "$(cat cmd.txt)" && sh cmd.txt
aws s3 ls s3://$B/            # empty: the file is "deleted"

./cloudundo log               # note the number (#)
./cloudundo undo 1            # preview, changes nothing
./cloudundo undo 1 --yes
aws s3 cp s3://$B/a.txt -     # must print: hello
```

## Test 2: overwrite a file, then undo

```sh
echo bye > b.txt
echo "aws s3 cp b.txt s3://$B/a.txt" > cmd.txt
./cloudundo protect -- "$(cat cmd.txt)" && sh cmd.txt
aws s3 cp s3://$B/a.txt -     # bye
./cloudundo undo 2 --yes      # use the number from the protect output
aws s3 cp s3://$B/a.txt -     # must print: hello
```

## Test 3: delete the whole bucket, then undo

```sh
echo "aws s3 rb s3://$B --force" > cmd.txt
./cloudundo protect -- "$(cat cmd.txt)" && sh cmd.txt
aws s3 ls s3://$B/            # error: bucket gone (if it says "BucketNotEmpty", that is a finding: send it to me)
./cloudundo undo 3 --yes
aws s3 cp s3://$B/a.txt -     # must print: hello
```

## Test 4: something cloudundo must block

```sh
echo 'aws s3 rm s3://$X --recursive' > cmd.txt
./cloudundo protect -- "$(cat cmd.txt)" && sh cmd.txt
echo "exit: $?"               # must be 2, and nothing ran
```

## Test 5: DynamoDB item

```sh
aws dynamodb create-table --table-name cu-test \
  --attribute-definitions AttributeName=pk,AttributeType=S \
  --key-schema AttributeName=pk,KeyType=HASH --billing-mode PAY_PER_REQUEST
aws dynamodb wait table-exists --table-name cu-test
aws dynamodb put-item --table-name cu-test --item '{"pk":{"S":"u1"},"name":{"S":"Ali"}}'

cat > cmd.txt <<'EOF'
aws dynamodb delete-item --table-name cu-test --key '{"pk":{"S":"u1"}}'
EOF
./cloudundo protect -- "$(cat cmd.txt)" && sh cmd.txt
./cloudundo undo 5 --yes      # use the number from the protect output
aws dynamodb get-item --table-name cu-test --key '{"pk":{"S":"u1"}}'   # must show Ali
```

## Test 6: DynamoDB table (takes a while)

```sh
echo "aws dynamodb delete-table --table-name cu-test" > cmd.txt
./cloudundo protect -- "$(cat cmd.txt)" && sh cmd.txt     # waits for the backup first
./cloudundo undo 6 --yes      # starts the restore
# wait 5–20 minutes, then:
./cloudundo undo 6 --yes      # puts the settings back, says "Restored"
aws dynamodb get-item --table-name cu-test --key '{"pk":{"S":"u1"}}'   # must show Ali
```

## Test 7: RDS database (about 30–40 minutes, mostly waiting)

Creates the smallest Postgres database, lets the "agent" delete it, and brings it back.
Cost: a few cents (or covered by your account's free credits). Delete it at the end.

```sh
aws rds create-db-instance --db-instance-identifier cu-test-db --engine postgres \
  --db-instance-class db.t4g.micro --allocated-storage 20 \
  --master-username cuadmin --master-user-password "$(openssl rand -hex 12)" \
  --no-publicly-accessible --backup-retention-period 1 --tags Key=purpose,Value=cloudundo-test > /dev/null
aws rds wait db-instance-available --db-instance-identifier cu-test-db      # 5–10 minutes

echo "aws rds delete-db-instance --db-instance-identifier cu-test-db --skip-final-snapshot" > cmd.txt
./cloudundo protect -- "$(cat cmd.txt)" && sh cmd.txt > /dev/null           # waits for the snapshot first
aws rds wait db-instance-deleted --db-instance-identifier cu-test-db        # 5–10 minutes

./cloudundo log                                   # note the number
./cloudundo undo <number> --yes                   # starts the restore
aws rds wait db-instance-available --db-instance-identifier cu-test-db      # 10–20 minutes
./cloudundo undo <number> --yes                   # finishes: says "Restored"
aws rds describe-db-instances --db-instance-identifier cu-test-db \
  --query 'DBInstances[0].[DBInstanceStatus,DBInstanceClass,BackupRetentionPeriod]'   # available, db.t4g.micro, 1
```

If `protect` says the snapshot is "still being made", run the same line again in a few minutes.

Clean up: `aws rds delete-db-instance --db-instance-identifier cu-test-db --skip-final-snapshot`, then in the RDS console under Snapshots delete `cloudundo-cu-test-db-…`.

## Clean up (in the AWS console)

- S3: for each `cloudundo-test-…` and `cloudundo-backup-…` bucket: Empty, then Delete.
- DynamoDB: delete `cu-test`, then Backups → delete the `cloudundo-…` backups.

## If anything looks wrong

Send me the full output (copy and paste, or a screenshot) plus `./cloudundo log --json`.
Remove the access key from `~/.aws/credentials` when you are done for the day.
