package awstest

import (
	"encoding/json"
	"fmt"
	"strings"

	"github.com/cloudundo/cloudundo/internal/awscli"
)

// CreateTable adds a table for a test.
func (f *Fake) CreateTable(name string, keyAttrs ...string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.nextID++
	f.tables[name] = &table{
		id:       fmt.Sprintf("tbl-%04d", f.nextID),
		arn:      fmt.Sprintf("arn:aws:dynamodb:us-east-1:%s:table/%s", f.Account, name),
		status:   "ACTIVE",
		keyAttrs: keyAttrs,
		items:    map[string]string{},
	}
}

// SetTableSettings sets the settings undo must bring back.
func (f *Fake) SetTableSettings(name, ttl string, tags map[string]string, pitr bool, stream string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	t := f.tables[name]
	t.ttl, t.pitr, t.stream = ttl, pitr, stream
	t.tags = nil
	for k, v := range tags {
		t.tags = append(t.tags, map[string]string{"Key": k, "Value": v})
	}
}

// TableSettings returns ttl, tags, pitr and stream of a table.
func (f *Fake) TableSettings(name string) (string, map[string]string, bool, string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	t := f.tables[name]
	tags := map[string]string{}
	for _, tg := range t.tags {
		tags[tg["Key"]] = tg["Value"]
	}
	return t.ttl, tags, t.pitr, t.stream
}

// TableID returns the table's unique id, or "".
func (f *Fake) TableID(name string) string {
	f.mu.Lock()
	defer f.mu.Unlock()
	if t := f.tables[name]; t != nil {
		return t.id
	}
	return ""
}

// TableArn returns the table's ARN, or "".
func (f *Fake) TableArn(name string) string {
	f.mu.Lock()
	defer f.mu.Unlock()
	if t := f.tables[name]; t != nil {
		return t.arn
	}
	return ""
}

// PutItem writes an item, as an app or agent would.
func (f *Fake) PutItem(tableName, item string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	t := f.tables[tableName]
	k, _ := t.keyOf(item)
	t.items[k] = compact(item)
}

// DeleteItem removes an item by key, as an agent would.
func (f *Fake) DeleteItem(tableName, key string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	t := f.tables[tableName]
	k, _ := t.keyOf(key)
	delete(t.items, k)
}

// Item returns the item with key, or "".
func (f *Fake) Item(tableName, key string) string {
	f.mu.Lock()
	defer f.mu.Unlock()
	t := f.tables[tableName]
	if t == nil {
		return ""
	}
	k, _ := t.keyOf(key)
	return t.items[k]
}

// DropTable deletes a table, as an agent would.
func (f *Fake) DropTable(name string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	delete(f.tables, name)
}

// HasTable reports whether the table exists.
func (f *Fake) HasTable(name string) bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.tables[name] != nil
}

// keyOf returns the canonical key of an item or key object.
func (t *table) keyOf(raw string) (string, error) {
	var m map[string]json.RawMessage
	if err := json.Unmarshal([]byte(raw), &m); err != nil {
		return "", err
	}
	k := map[string]json.RawMessage{}
	for _, a := range t.keyAttrs {
		v, ok := m[a]
		if !ok {
			return "", fmt.Errorf("missing key attribute %s", a)
		}
		k[a] = json.RawMessage(compact(string(v)))
	}
	b, err := json.Marshal(k)
	return string(b), err
}

func compact(s string) string {
	var v any
	if json.Unmarshal([]byte(s), &v) != nil {
		return s
	}
	b, _ := json.Marshal(v)
	return string(b)
}

func (f *Fake) dynamo(op string, flags map[string]string, args []string) ([]byte, error) {
	errOut := func(msg string) ([]byte, error) {
		return nil, &awscli.Error{Args: args, Code: 254, Stderr: msg}
	}
	js := func(v any) ([]byte, error) { return json.Marshal(v) }
	name := flags["table-name"]
	t := f.tables[name]
	notFound := func() ([]byte, error) {
		return errOut("An error occurred (ResourceNotFoundException) when calling the operation: Requested resource not found: Table: " + name + " not found")
	}

	switch op {
	case "describe-table":
		if t == nil {
			return notFound()
		}
		status := t.status
		if t.creatingPolls > 0 {
			t.creatingPolls--
			status = "CREATING"
		}
		var ks []map[string]string
		for i, a := range t.keyAttrs {
			kt := "HASH"
			if i > 0 {
				kt = "RANGE"
			}
			ks = append(ks, map[string]string{"AttributeName": a, "KeyType": kt})
		}
		desc := map[string]any{"TableId": t.id, "TableArn": t.arn, "TableStatus": status, "KeySchema": ks,
			"DeletionProtectionEnabled": t.protection}
		if t.stream != "" {
			desc["StreamSpecification"] = map[string]any{"StreamEnabled": true, "StreamViewType": t.stream}
		}
		return js(map[string]any{"Table": desc})

	case "get-item":
		if t == nil {
			return notFound()
		}
		k, err := t.keyOf(flags["key"])
		if err != nil {
			return errOut("An error occurred (ValidationException): " + err.Error())
		}
		if it, ok := t.items[k]; ok {
			return []byte(`{"Item":` + it + `}`), nil
		}
		return []byte("{}"), nil

	case "put-item":
		if t == nil {
			return notFound()
		}
		k, err := t.keyOf(flags["item"])
		if err != nil {
			return errOut("An error occurred (ValidationException): " + err.Error())
		}
		t.items[k] = compact(flags["item"])
		return []byte("{}"), nil

	case "create-backup":
		if t == nil {
			return notFound()
		}
		f.nextID++
		arn := fmt.Sprintf("%s/backup/%d", t.arn, f.nextID)
		snap := *t
		snap.items = map[string]string{}
		for k, v := range t.items {
			snap.items[k] = v
		}
		f.backups[arn] = &backup{table: &snap, name: flags["backup-name"], polls: f.BackupPolls}
		status := "AVAILABLE"
		if f.BackupPolls > 0 {
			status = "CREATING"
		}
		return js(map[string]any{"BackupDetails": map[string]string{"BackupArn": arn, "BackupStatus": status}})

	case "delete-backup":
		if f.backups[flags["backup-arn"]] == nil {
			return errOut("An error occurred (BackupNotFoundException)")
		}
		delete(f.backups, flags["backup-arn"])
		return js(map[string]any{"BackupDescription": map[string]any{"BackupDetails": map[string]string{"BackupStatus": "DELETED"}}})

	case "describe-backup":
		b := f.backups[flags["backup-arn"]]
		if b == nil {
			return errOut("An error occurred (BackupNotFoundException)")
		}
		status := "AVAILABLE"
		if b.polls > 0 {
			b.polls--
			status = "CREATING"
		}
		return js(map[string]any{"BackupDescription": map[string]any{"BackupDetails": map[string]string{"BackupStatus": status}}})

	case "restore-table-from-backup":
		target := flags["target-table-name"]
		if f.tables[target] != nil {
			return errOut("An error occurred (TableAlreadyExistsException): Table already exists: " + target)
		}
		b := f.backups[flags["backup-arn"]]
		if b == nil {
			return errOut("An error occurred (BackupNotFoundException)")
		}
		f.nextID++
		restored := &table{
			// Like AWS: same name, same ARN, new id.
			id:            fmt.Sprintf("tbl-%04d", f.nextID),
			arn:           fmt.Sprintf("arn:aws:dynamodb:us-east-1:%s:table/%s", f.Account, target),
			status:        "ACTIVE",
			keyAttrs:      b.table.keyAttrs,
			items:         map[string]string{},
			creatingPolls: f.RestorePolls,
		}
		// Like AWS: data and keys come back, settings do not.
		for k, v := range b.table.items {
			restored.items[k] = v
		}
		f.tables[target] = restored
		return js(map[string]any{"TableDescription": map[string]string{"TableId": restored.id, "TableArn": restored.arn, "TableStatus": "CREATING"}})

	case "describe-time-to-live":
		if t == nil {
			return notFound()
		}
		if t.ttl == "" {
			return js(map[string]any{"TimeToLiveDescription": map[string]string{"TimeToLiveStatus": "DISABLED"}})
		}
		return js(map[string]any{"TimeToLiveDescription": map[string]string{"TimeToLiveStatus": "ENABLED", "AttributeName": t.ttl}})

	case "update-time-to-live":
		if t == nil {
			return notFound()
		}
		spec := flags["time-to-live-specification"]
		if i := strings.Index(spec, "AttributeName="); i >= 0 && strings.Contains(spec, "Enabled=true") {
			t.ttl = spec[i+len("AttributeName="):]
		}
		return nil, nil

	case "list-tags-of-resource":
		for _, tb := range f.tables {
			if tb.arn == flags["resource-arn"] {
				return js(map[string]any{"Tags": tb.tags})
			}
		}
		return errOut("An error occurred (ResourceNotFoundException)")

	case "tag-resource":
		var tags []map[string]string
		if err := json.Unmarshal([]byte(flags["tags"]), &tags); err != nil {
			return errOut("bad tags")
		}
		for _, tb := range f.tables {
			if tb.arn == flags["resource-arn"] {
				tb.tags = append(tb.tags, tags...)
				return nil, nil
			}
		}
		return errOut("An error occurred (ResourceNotFoundException)")

	case "describe-continuous-backups":
		if t == nil {
			return notFound()
		}
		st := "DISABLED"
		if t.pitr {
			st = "ENABLED"
		}
		return js(map[string]any{"ContinuousBackupsDescription": map[string]any{
			"PointInTimeRecoveryDescription": map[string]string{"PointInTimeRecoveryStatus": st}}})

	case "update-continuous-backups":
		if t == nil {
			return notFound()
		}
		t.pitr = strings.Contains(flags["point-in-time-recovery-specification"], "true")
		return nil, nil

	case "update-table":
		if t == nil {
			return notFound()
		}
		if spec := flags["stream-specification"]; spec != "" {
			if i := strings.Index(spec, "StreamViewType="); i >= 0 {
				t.stream = spec[i+len("StreamViewType="):]
			}
		}
		if flags["deletion-protection-enabled"] == "true" {
			t.protection = true
		}
		return nil, nil
	}
	return errOut("fake aws: unsupported dynamodb call " + op)
}

// HasBackup reports whether a DynamoDB backup exists.
func (f *Fake) HasBackup(arn string) bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.backups[arn] != nil
}
