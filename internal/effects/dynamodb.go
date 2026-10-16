package effects

import (
	"bytes"
	"encoding/json"
	"regexp"
	"sort"
	"strings"
)

// DynamoDB kinds.
const (
	// DeleteTable removes a table and every item in it.
	DeleteTable Kind = "delete-table"
	// DeleteItems removes the items whose keys are in Items.
	DeleteItems Kind = "delete-items"
	// OverwriteItems replaces or changes items (put-item, update-item).
	OverwriteItems Kind = "overwrite-items"
)

const unsupportedStatement = "PartiQL statements and transactions are not supported yet; use delete-item, put-item or update-item"

// dynamo handles "aws dynamodb <operation>".
func dynamo(p parsed) []Effect {
	op := p.pos[1]
	table := p.value("table-name")

	switch op {
	case "delete-table":
		return []Effect{{Kind: DeleteTable, Table: table}}

	case "delete-item":
		return []Effect{itemEffect(DeleteItems, table, p.value("key"), false)}
	case "update-item":
		return []Effect{itemEffect(OverwriteItems, table, p.value("key"), false)}
	case "put-item":
		return []Effect{itemEffect(OverwriteItems, table, p.value("item"), true)}

	case "batch-write-item":
		return batchWrite(p.value("request-items"))

	case "execute-statement":
		stmt := strings.ToUpper(strings.TrimSpace(p.value("statement")))
		if strings.HasPrefix(stmt, "SELECT") {
			return nil
		}
		return []Effect{{Kind: DeleteItems, Table: "?", Uncertain: true, Detail: unsupportedStatement}}
	case "batch-execute-statement", "execute-transaction", "transact-write-items":
		return []Effect{{Kind: DeleteItems, Table: "?", Uncertain: true, Detail: unsupportedStatement}}

	case "update-table":
		if p.has("no-deletion-protection-enabled") {
			return []Effect{{Kind: WeakenProtection, Table: table, Detail: "turns off deletion protection"}}
		}
	case "update-continuous-backups":
		spec := strings.ToLower(strings.ReplaceAll(p.value("point-in-time-recovery-specification"), " ", ""))
		if strings.Contains(spec, "false") {
			return []Effect{{Kind: WeakenProtection, Table: table, Detail: "turns off point-in-time recovery"}}
		}
	case "update-time-to-live":
		spec := strings.ToLower(strings.ReplaceAll(p.value("time-to-live-specification"), " ", ""))
		if strings.Contains(spec, "true") {
			return []Effect{{Kind: WeakenProtection, Table: table, Detail: "turns on TTL, so DynamoDB starts deleting expired items by itself"}}
		}
	case "delete-backup":
		return []Effect{{Kind: WeakenProtection, Table: "?", Detail: "deletes a backup"}}
	}
	return nil
}

// itemEffect reads one --key or --item JSON argument.
func itemEffect(k Kind, table, raw string, full bool) Effect {
	e := Effect{Kind: k, Table: table, FullItems: full}
	item, ok := compactObject(raw)
	if !ok {
		e.Uncertain = true
		e.Detail = "the item key is not written as JSON in the command"
		return e
	}
	e.Items = []string{item}
	return e
}

// batchWrite reads --request-items: {"Table":[{"DeleteRequest":{"Key":{}}},{"PutRequest":{"Item":{}}}]}.
func batchWrite(raw string) []Effect {
	if _, ok := compactObject(raw); !ok {
		return []Effect{{Kind: DeleteItems, Table: "?", Uncertain: true,
			Detail: "the request items are not written as JSON in the command"}}
	}
	var req map[string][]struct {
		DeleteRequest *struct{ Key json.RawMessage }
		PutRequest    *struct{ Item json.RawMessage }
	}
	if err := json.Unmarshal([]byte(raw), &req); err != nil {
		return []Effect{{Kind: DeleteItems, Table: "?", Uncertain: true, Detail: "could not read the request items"}}
	}
	tables := make([]string, 0, len(req))
	for t := range req {
		tables = append(tables, t)
	}
	sort.Strings(tables)

	var out []Effect
	for _, t := range tables {
		del := Effect{Kind: DeleteItems, Table: t}
		put := Effect{Kind: OverwriteItems, Table: t, FullItems: true}
		for _, r := range req[t] {
			switch {
			case r.DeleteRequest != nil:
				k, ok := compactObject(string(r.DeleteRequest.Key))
				if !ok {
					del.Uncertain = true
				}
				del.Items = append(del.Items, k)
			case r.PutRequest != nil:
				it, ok := compactObject(string(r.PutRequest.Item))
				if !ok {
					put.Uncertain = true
				}
				put.Items = append(put.Items, it)
			}
		}
		if len(del.Items) > 0 || del.Uncertain {
			out = append(out, del)
		}
		if len(put.Items) > 0 || put.Uncertain {
			out = append(out, put)
		}
	}
	return out
}

// compactObject returns raw as compact JSON if it is a non-empty object.
func compactObject(raw string) (string, bool) {
	raw = strings.TrimSpace(raw)
	if raw == "" || !strings.HasPrefix(raw, "{") {
		return "", false
	}
	var m map[string]json.RawMessage
	if json.Unmarshal([]byte(raw), &m) != nil || len(m) == 0 {
		return "", false
	}
	var b bytes.Buffer
	if json.Compact(&b, []byte(raw)) != nil {
		return "", false
	}
	return b.String(), true
}

var (
	tableName = regexp.MustCompile(`^[A-Za-z0-9_.-]{1,255}$`)
	tableArn  = regexp.MustCompile(`^arn:aws[a-z-]*:dynamodb:[a-z0-9-]+:[0-9]{12}:table/[A-Za-z0-9_.-]{1,255}$`)
)

// validTable reports whether name is a table name or ARN DynamoDB would
// accept, written out in full. Anything else (a variable, a shell glob,
// a placeholder) means cloudundo does not know which table is meant.
func validTable(name string) bool {
	return tableName.MatchString(name) || tableArn.MatchString(name)
}
