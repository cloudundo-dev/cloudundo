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
aws configure --profile cloudundo-test      # paste the key, pick a region (for example us-west-1), output: json
export AWS_PROFILE=cloudundo-test
aws sts get-caller-identity                  # MUST show the NEW account number
```

5. Pick the file for your computer, rename it, and make it runnable:

```sh
# Mac with Apple chip: cloudundo-darwin-arm64   | Intel Mac: cloudundo-darwin-amd64 | Linux/WSL: cloudundo-linux-amd64
mv cloudundo-darwin-arm64 cloudundo
chmod +x cloudundo
xattr -d com.apple.quarantine cloudundo 2>/dev/null   # Mac only
./cloudundo version                                    # prints "cloudundo" and a version
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

## Test 7: Terraform destroys a database, a bucket and a table (about 40 minutes, mostly waiting)

This is the Claude Code + Terraform incident (reported March 2026) in small: an agent runs `terraform destroy` on an RDS database and its data.
It tests RDS and Terraform together. Cost: a few cents (or your account's free credits). Needs Terraform
installed in Ubuntu (`terraform -version`).

First give the test user RDS rights (it only has S3 and DynamoDB from step 0): in the console, IAM → Users →
`cloudundo-test` → Add permissions → Attach policies directly → `AmazonRDSFullAccess`. Remove it again after the clean-up.
Everything below runs in the region you gave `aws configure`; the `AWS_REGION` line passes it to Terraform too.

```sh
export AWS_PROFILE=cloudundo-test                     # the TEST account, for aws and terraform
aws sts get-caller-identity                            # check the account number
export AWS_REGION=$(aws configure get region)          # the same region for aws and terraform
echo $AWS_REGION                                       # for example us-west-1
mkdir -p ~/cloudundo-test/tf && cd ~/cloudundo-test/tf
cat > main.tf <<'TF'
provider "aws" {}                         # region comes from AWS_REGION
variable "db_password" { sensitive = true }
resource "aws_db_instance" "main" {
  identifier          = "cu-tf-db"
  engine              = "postgres"
  instance_class      = "db.t3.micro"
  allocated_storage   = 20
  username            = "cuadmin"
  password            = var.db_password
  skip_final_snapshot = true
  publicly_accessible = false
  tags = { purpose = "cloudundo-test" }
}
resource "aws_s3_bucket" "files" {
  bucket_prefix = "cu-tf-files-"
  force_destroy = true
}
resource "aws_s3_object" "hello" {
  bucket  = aws_s3_bucket.files.id
  key     = "hello.txt"
  content = "hello"
}
resource "aws_dynamodb_table" "users" {
  name         = "cu-tf-users"
  billing_mode = "PAY_PER_REQUEST"
  hash_key     = "pk"
  attribute {
    name = "pk"
    type = "S"
  }
}
output "bucket" { value = aws_s3_bucket.files.id }
TF
export TF_VAR_db_password="$(openssl rand -hex 12)"
terraform init -input=false
terraform apply -auto-approve                       # 5–10 minutes
BUCKET=$(terraform output -raw bucket)
aws dynamodb put-item --table-name cu-tf-users --item '{"pk":{"S":"u1"},"name":{"S":"Ali"}}'

# The "agent":
../cloudundo protect -- "terraform destroy -auto-approve" && terraform destroy -auto-approve
```

What to look for in cloudundo's output: a snapshot of `cu-tf-db`, a copy of the bucket, a backup of the table.
Then the destroy runs (5–10 minutes).

```sh
../cloudundo log                                   # note the number
../cloudundo undo <number> --yes                   # starts the database and table restores, copies the bucket back
aws rds wait db-instance-available --db-instance-identifier cu-tf-db       # 10–20 minutes
../cloudundo undo <number> --yes                   # finishes: says "Restored"

aws s3 cp s3://$BUCKET/hello.txt -                 # hello
aws dynamodb get-item --table-name cu-tf-users --key '{"pk":{"S":"u1"}}'   # Ali
aws rds describe-db-instances --db-instance-identifier cu-tf-db --query 'DBInstances[0].[DBInstanceStatus,DBInstanceClass]'
```

If `protect` says the snapshot is "still being made", run the same line again in a few minutes.

Clean up. Terraform thinks these are gone, so delete them directly:

```sh
aws rds delete-db-instance --db-instance-identifier cu-tf-db --skip-final-snapshot
aws dynamodb delete-table --table-name cu-tf-users
aws s3 rb s3://$BUCKET --force
```

Then in the console: RDS → Snapshots → delete `cloudundo-cu-tf-db-…`; DynamoDB → Backups → delete `cloudundo-cu-tf-users-…`;
S3 → empty and delete `cloudundo-backup-…`.

## Test 8: an RDS delete through the AWS CLI (about 30 minutes, mostly waiting)

Done on 22 October 2026: protected, deleted, restored with its backup setting.

```sh
aws rds create-db-instance --db-instance-identifier cu-cli-db --engine postgres --db-instance-class db.t3.micro --allocated-storage 20 --master-username cuadmin --master-user-password "$(openssl rand -hex 12)" --no-publicly-accessible --backup-retention-period 1 --query 'DBInstance.DBInstanceStatus'
aws rds wait db-instance-available --db-instance-identifier cu-cli-db && echo READY
./cloudundo protect -- "aws rds delete-db-instance --db-instance-identifier cu-cli-db --skip-final-snapshot"
aws rds delete-db-instance --db-instance-identifier cu-cli-db --skip-final-snapshot --query 'DBInstance.DBInstanceStatus'
aws rds wait db-instance-deleted --db-instance-identifier cu-cli-db && echo DELETED
./cloudundo undo <number> --yes
aws rds wait db-instance-available --db-instance-identifier cu-cli-db && echo READY
./cloudundo undo <number> --yes
aws rds describe-db-instances --db-instance-identifier cu-cli-db --query 'DBInstances[0].[DBInstanceStatus,DBInstanceClass,BackupRetentionPeriod]' --output text
```

## Test 9: CloudFormation, a deploy that drops a table and a stack delete (about 15 minutes)

Done on 24 October 2026: the deploy's change set was read and deleted again, the table was backed up and restored, the stack delete's bucket was copied and restored, and the retained table was left alone.

A stack with a bucket, a table, and a second table the template keeps (`DeletionPolicy: Retain`).
No database, so it is quick: databases go through the same protection Tests 7 and 8 checked.

```sh
mkdir -p ~/cloudundo-test/cfn && cd ~/cloudundo-test/cfn
cat > v1.json <<'JSON'
{"Resources": {
  "Files": {"Type": "AWS::S3::Bucket"},
  "Users": {"Type": "AWS::DynamoDB::Table", "Properties": {"BillingMode": "PAY_PER_REQUEST",
    "AttributeDefinitions": [{"AttributeName": "pk", "AttributeType": "S"}],
    "KeySchema": [{"AttributeName": "pk", "KeyType": "HASH"}]}},
  "Orders": {"Type": "AWS::DynamoDB::Table", "DeletionPolicy": "Retain", "Properties": {"BillingMode": "PAY_PER_REQUEST",
    "AttributeDefinitions": [{"AttributeName": "pk", "AttributeType": "S"}],
    "KeySchema": [{"AttributeName": "pk", "KeyType": "HASH"}]}}
}}
JSON
python3 -c "import json; t=json.load(open('v1.json')); del t['Resources']['Users']; json.dump(t, open('v2.json','w'))"

../cloudundo protect -- "aws cloudformation deploy --template-file v1.json --stack-name cu-cfn" && aws cloudformation deploy --template-file v1.json --stack-name cu-cfn
res() { aws cloudformation describe-stack-resource --stack-name cu-cfn --logical-resource-id $1 --query StackResourceDetail.PhysicalResourceId --output text; }
B=$(res Files); USERS=$(res Users); ORDERS=$(res Orders); echo $B $USERS $ORDERS
echo hello > a.txt && aws s3 cp a.txt s3://$B/a.txt
aws dynamodb put-item --table-name $USERS --item '{"pk":{"S":"u1"},"name":{"S":"Ali"}}'
```

The first `protect` should say the stack does not exist yet, so the deploy only creates resources.

**9a. A deploy that drops the Users table:**

```sh
../cloudundo protect -- "aws cloudformation deploy --template-file v2.json --stack-name cu-cfn"
```

Look for: a backup of the Users table, and nothing about the bucket. Then:

```sh
aws cloudformation list-change-sets --stack-name cu-cfn --query 'length(Summaries)'   # 0: cloudundo removed the change set it made
aws cloudformation deploy --template-file v2.json --stack-name cu-cfn                  # deletes the Users table
../cloudundo undo <number> --yes          # starts the table restore
sleep 120; ../cloudundo undo <number> --yes
aws dynamodb get-item --table-name $USERS --key '{"pk":{"S":"u1"}}' --query 'Item.name.S' --output text   # Ali
```

**9b. The agent empties the bucket and deletes the stack:**

```sh
../cloudundo protect -- "aws s3 rm s3://$B --recursive && aws cloudformation delete-stack --stack-name cu-cfn"
```

Look for: versioning and a copy of the bucket, and a note that the template keeps `cu-cfn/Orders`. Then:

```sh
aws s3 rm s3://$B --recursive && aws cloudformation delete-stack --stack-name cu-cfn
aws cloudformation wait stack-delete-complete --stack-name cu-cfn && echo DELETED
../cloudundo undo <number> --yes
aws s3 cp s3://$B/a.txt -                       # hello
aws dynamodb describe-table --table-name $ORDERS --query 'Table.TableStatus'   # ACTIVE: kept by the template
```

## Test 10: cleanup, which also cleans up after all the tests

Done on 24 October 2026: nine backups listed, the agent's cleanup blocked, all nine deleted (including ones already removed by hand).

```sh
cd ~/cloudundo-test
./cloudundo cleanup                         # lists every backup the tests left; deletes nothing
./cloudundo protect -- "./cloudundo cleanup --all --yes"; echo "exit $?"   # 2: an agent may not run it
./cloudundo cleanup --all --yes             # you may
./cloudundo cleanup                         # No backups to clean up.
```

Then delete what the tests restored: the Users and Orders tables, and the bucket (`aws s3 rb s3://$B --force`, then empty and delete the versions in the console if it says the bucket is not empty).

## Clean up (in the AWS console)

- S3: for each `cloudundo-test-…` and `cloudundo-backup-…` bucket: Empty, then Delete.
- DynamoDB: delete `cu-test`, then Backups → delete the `cloudundo-…` backups.

## If anything looks wrong

Send me the full output (copy and paste, or a screenshot) plus `./cloudundo log --json`.
Remove the access key from `~/.aws/credentials` when you are done for the day.
