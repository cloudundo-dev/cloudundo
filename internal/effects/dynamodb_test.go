package effects

import (
	"reflect"
	"testing"
)

func TestAnalyzeDynamoDB(t *testing.T) {
	tests := []struct {
		name string
		line string
		want []Effect
	}{
		{
			name: "delete table",
			line: "aws dynamodb delete-table --table-name Users",
			want: []Effect{{Kind: DeleteTable, Table: "Users"}},
		},
		{
			name: "delete one item",
			line: `aws dynamodb delete-item --table-name Users --key '{"pk": {"S": "u#1"}}'`,
			want: []Effect{{Kind: DeleteItems, Table: "Users", Items: []string{`{"pk":{"S":"u#1"}}`}}},
		},
		{
			name: "update item",
			line: `aws dynamodb update-item --table-name Users --key '{"pk":{"S":"u#1"}}' --update-expression "SET plan = :p" --expression-attribute-values '{":p":{"S":"pro"}}'`,
			want: []Effect{{Kind: OverwriteItems, Table: "Users", Items: []string{`{"pk":{"S":"u#1"}}`}}},
		},
		{
			name: "put item carries the whole item",
			line: `aws dynamodb put-item --table-name Users --item '{"pk":{"S":"u#2"},"name":{"S":"Sara"}}'`,
			want: []Effect{{Kind: OverwriteItems, Table: "Users", Items: []string{`{"pk":{"S":"u#2"},"name":{"S":"Sara"}}`}, FullItems: true}},
		},
		{
			name: "dollar signs inside single-quoted JSON are data",
			line: `aws dynamodb put-item --table-name Prices --item '{"pk":{"S":"p1"},"price":{"S":"$5"}}'`,
			want: []Effect{{Kind: OverwriteItems, Table: "Prices", Items: []string{`{"pk":{"S":"p1"},"price":{"S":"$5"}}`}, FullItems: true}},
		},
		{
			name: "batch write across two tables",
			line: `aws dynamodb batch-write-item --request-items '{"Orders":[{"DeleteRequest":{"Key":{"id":{"N":"1"}}}},{"PutRequest":{"Item":{"id":{"N":"2"},"total":{"N":"9"}}}}],"Carts":[{"DeleteRequest":{"Key":{"id":{"S":"c"}}}}]}'`,
			want: []Effect{
				{Kind: DeleteItems, Table: "Carts", Items: []string{`{"id":{"S":"c"}}`}},
				{Kind: DeleteItems, Table: "Orders", Items: []string{`{"id":{"N":"1"}}`}},
				{Kind: OverwriteItems, Table: "Orders", Items: []string{`{"id":{"N":"2"},"total":{"N":"9"}}`}, FullItems: true},
			},
		},
		{
			name: "key from a file is uncertain",
			line: "aws dynamodb delete-item --table-name Users --key file://key.json",
			want: []Effect{{Kind: DeleteItems, Table: "Users", Uncertain: true, Detail: "the item key is not written as JSON in the command"}},
		},
		{
			name: "key from a variable is uncertain",
			line: `aws dynamodb delete-item --table-name Users --key "$KEY"`,
			want: []Effect{{Kind: DeleteItems, Table: "Users", Uncertain: true, Detail: "the item key is not written as JSON in the command"}},
		},
		{
			name: "table from a variable is uncertain",
			line: "aws dynamodb delete-table --table-name $TABLE",
			want: []Effect{{Kind: DeleteTable, Table: "$TABLE", Uncertain: true}},
		},
		{
			name: "PartiQL delete is not supported",
			line: `aws dynamodb execute-statement --statement "DELETE FROM Users WHERE pk='u#1'"`,
			want: []Effect{{Kind: DeleteItems, Table: "?", Uncertain: true, Detail: unsupportedStatement}},
		},
		{
			name: "PartiQL select is harmless",
			line: `aws dynamodb execute-statement --statement "SELECT * FROM Users"`,
			want: nil,
		},
		{
			name: "transactions are not supported",
			line: "aws dynamodb transact-write-items --transact-items file://tx.json",
			want: []Effect{{Kind: DeleteItems, Table: "?", Uncertain: true, Detail: unsupportedStatement}},
		},
		{
			name: "turning off deletion protection",
			line: "aws dynamodb update-table --table-name Users --no-deletion-protection-enabled",
			want: []Effect{{Kind: WeakenProtection, Table: "Users", Detail: "turns off deletion protection"}},
		},
		{
			name: "turning on deletion protection is fine",
			line: "aws dynamodb update-table --table-name Users --deletion-protection-enabled",
			want: nil,
		},
		{
			name: "turning off PITR",
			line: "aws dynamodb update-continuous-backups --table-name Users --point-in-time-recovery-specification PointInTimeRecoveryEnabled=false",
			want: []Effect{{Kind: WeakenProtection, Table: "Users", Detail: "turns off point-in-time recovery"}},
		},
		{
			name: "turning on TTL deletes expired items",
			line: "aws dynamodb update-time-to-live --table-name Sessions --time-to-live-specification Enabled=true,AttributeName=ttl",
			want: []Effect{{Kind: WeakenProtection, Table: "Sessions", Detail: "turns on TTL, so DynamoDB starts deleting expired items by itself"}},
		},
		{
			name: "deleting a backup",
			line: "aws dynamodb delete-backup --backup-arn arn:aws:dynamodb:us-east-1:1:table/T/backup/x",
			want: []Effect{{Kind: WeakenProtection, Table: "?", Uncertain: true, Detail: "deletes a backup"}},
		},
		{
			name: "reads are harmless",
			line: `aws dynamodb get-item --table-name Users --key '{"pk":{"S":"a"}}' --consistent-read && aws dynamodb scan --table-name Users`,
			want: nil,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := strip(Analyze(tt.line))
			if !reflect.DeepEqual(got, tt.want) {
				t.Fatalf("Analyze(%q)\n got  %+v\n want %+v", tt.line, got, tt.want)
			}
		})
	}
}

func TestDynamoServiceIsSet(t *testing.T) {
	e := Analyze("aws dynamodb delete-table --table-name T")
	if len(e) != 1 || e[0].Service != "dynamodb" {
		t.Fatalf("%+v", e)
	}
	if e := Analyze("aws s3 rm s3://b/k"); e[0].Service != "s3" {
		t.Fatalf("%+v", e)
	}
}

func TestTableNamesMustBeWrittenOut(t *testing.T) {
	for line, uncertain := range map[string]bool{
		"aws dynamodb delete-table --table-name Users":                                               false,
		"aws dynamodb delete-table --table-name arn:aws:dynamodb:us-east-1:123456789012:table/Users": false,
		"aws dynamodb delete-table --table-name ?":                                                   true,
		"aws dynamodb delete-table --table-name Use*":                                                true,
		"aws dynamodb delete-table --table-name 'a b'":                                               true,
		"aws dynamodb delete-table --table-name $T":                                                  true,
	} {
		effs := Analyze(line)
		if len(effs) != 1 || effs[0].Uncertain != uncertain {
			t.Errorf("%s: %+v", line, effs)
		}
	}
}
