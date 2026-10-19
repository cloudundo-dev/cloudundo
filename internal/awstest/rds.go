package awstest

import (
	"encoding/json"
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/cloudundo/cloudundo/internal/awscli"
)

type fdb struct {
	info awscli.DBInfo
	data string
	// creatingPolls is how many more describe calls report "creating".
	creatingPolls int
}

type fsnap struct {
	db      awscli.DBInfo
	data    string
	created time.Time
	polls   int
}

type fcache struct {
	engine     string
	group      string
	serverless bool
}

func (f *Fake) rdsInit() {
	if f.dbs == nil {
		f.dbs = map[string]*fdb{}
		f.clusters = map[string]*fdb{}
		f.dbSnaps = map[string]*fsnap{}
		f.caches = map[string]*fcache{}
	}
}

// CreateDB adds an RDS instance (or, with info.Cluster, an Aurora
// cluster) holding data, for a test.
func (f *Fake) CreateDB(info awscli.DBInfo, data string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.rdsInit()
	kind := "db"
	if info.Cluster {
		kind = "cluster"
	}
	info.Arn = fmt.Sprintf("arn:aws:rds:us-east-1:%s:%s:%s", f.Account, kind, info.ID)
	f.nextID++
	info.ResourceID = fmt.Sprintf("%s-res-%04d", kind, f.nextID)
	info.Status = "available"
	if info.Cluster {
		f.clusters[info.ID] = &fdb{info: info, data: data}
		return
	}
	f.dbs[info.ID] = &fdb{info: info, data: data}
	if c := f.clusters[info.ClusterID]; info.ClusterID != "" && c != nil {
		c.info.Members = append(c.info.Members, awscli.DBMember{ID: info.ID})
	}
}

// DB returns an instance (or cluster) and its data.
func (f *Fake) DB(id string, cluster bool) (awscli.DBInfo, string, bool) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.rdsInit()
	m := f.dbs
	if cluster {
		m = f.clusters
	}
	d := m[id]
	if d == nil {
		return awscli.DBInfo{}, "", false
	}
	return d.info, d.data, true
}

// DropDB deletes a database as an agent would. With final set, AWS keeps
// a final snapshot of that name.
func (f *Fake) DropDB(id string, cluster bool, final string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.rdsInit()
	m := f.dbs
	if cluster {
		m = f.clusters
	}
	if d := m[id]; d != nil && final != "" {
		f.dbSnaps[final] = &fsnap{db: d.info, data: d.data, created: f.tick(), polls: f.SnapshotPolls}
	}
	delete(m, id)
}

// HasDBSnapshot reports whether a snapshot exists.
func (f *Fake) HasDBSnapshot(id string) bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.rdsInit()
	return f.dbSnaps[id] != nil
}

// CreateCache adds an ElastiCache cluster or serverless cache.
func (f *Fake) CreateCache(id, engine, group string, serverless bool) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.rdsInit()
	f.caches[id] = &fcache{engine: engine, group: group, serverless: serverless}
}

func multi(args []string, name string) []string {
	var out []string
	for i := 0; i < len(args); i++ {
		if args[i] == "--"+name {
			for j := i + 1; j < len(args) && !strings.HasPrefix(args[j], "--"); j++ {
				out = append(out, args[j])
			}
		}
	}
	return out
}

func describeInstance(d *fdb, status string) map[string]any {
	i := d.info
	sgs := []map[string]string{}
	for _, g := range i.SecurityGroups {
		sgs = append(sgs, map[string]string{"VpcSecurityGroupId": g})
	}
	out := map[string]any{
		"DBInstanceIdentifier": i.ID, "DBInstanceArn": i.Arn, "DbiResourceId": i.ResourceID, "DBInstanceStatus": status,
		"DBInstanceClass": i.Class, "Engine": i.Engine, "EngineVersion": i.EngineVersion,
		"StorageType": i.StorageType, "MultiAZ": i.MultiAZ, "PubliclyAccessible": i.Public,
		"DeletionProtection": i.DeletionProtection, "BackupRetentionPeriod": i.BackupRetention,
		"VpcSecurityGroups": sgs, "TagList": i.Tags,
		"Endpoint": map[string]int{"Port": i.Port},
	}
	if i.SubnetGroup != "" {
		out["DBSubnetGroup"] = map[string]string{"DBSubnetGroupName": i.SubnetGroup}
	}
	if i.ParameterGroup != "" {
		out["DBParameterGroups"] = []map[string]string{{"DBParameterGroupName": i.ParameterGroup}}
	}
	if i.OptionGroup != "" {
		out["OptionGroupMemberships"] = []map[string]string{{"OptionGroupName": i.OptionGroup}}
	}
	if i.ClusterID != "" {
		out["DBClusterIdentifier"] = i.ClusterID
	}
	return out
}

func describeCluster(d *fdb, status string) map[string]any {
	i := d.info
	sgs := []map[string]string{}
	for _, g := range i.SecurityGroups {
		sgs = append(sgs, map[string]string{"VpcSecurityGroupId": g})
	}
	members := []map[string]any{}
	for _, m := range i.Members {
		members = append(members, map[string]any{"DBInstanceIdentifier": m.ID, "IsClusterWriter": m.Writer})
	}
	return map[string]any{
		"DBClusterIdentifier": i.ID, "DBClusterArn": i.Arn, "DbClusterResourceId": i.ResourceID, "Status": status, "Engine": i.Engine,
		"EngineVersion": i.EngineVersion, "DBSubnetGroup": i.SubnetGroup,
		"DBClusterParameterGroup": i.ParameterGroup, "VpcSecurityGroups": sgs, "Port": i.Port,
		"DeletionProtection": i.DeletionProtection, "BackupRetentionPeriod": i.BackupRetention,
		"DBClusterMembers": members, "TagList": i.Tags,
	}
}

func (d *fdb) status() string {
	if d.creatingPolls > 0 {
		d.creatingPolls--
		return "creating"
	}
	return d.info.Status
}

func (s *fsnap) status() string {
	if s.polls > 0 {
		s.polls--
		return "creating"
	}
	return "available"
}

func (f *Fake) rdsCall(op string, flags map[string]string, args []string) ([]byte, error) {
	f.rdsInit()
	errOut := func(msg string) ([]byte, error) {
		return nil, &awscli.Error{Args: args, Code: 254, Stderr: "An error occurred (" + msg + ")"}
	}
	js := func(v any) ([]byte, error) { return json.Marshal(v) }
	inst, cl := flags["db-instance-identifier"], flags["db-cluster-identifier"]

	switch op {
	case "describe-db-instances":
		d := f.dbs[inst]
		if d == nil {
			return errOut("DBInstanceNotFound) when calling the DescribeDBInstances operation: DBInstance " + inst + " not found.")
		}
		return js(map[string]any{"DBInstances": []any{describeInstance(d, d.status())}})

	case "describe-db-clusters":
		d := f.clusters[cl]
		if d == nil {
			return errOut("DBClusterNotFoundFault) when calling the DescribeDBClusters operation: DBCluster " + cl + " not found.")
		}
		return js(map[string]any{"DBClusters": []any{describeCluster(d, d.status())}})

	case "create-db-snapshot", "create-db-cluster-snapshot":
		cluster := op == "create-db-cluster-snapshot"
		src, id := f.dbs[inst], flags["db-snapshot-identifier"]
		if cluster {
			src, id = f.clusters[cl], flags["db-cluster-snapshot-identifier"]
		}
		if src == nil {
			return errOut("DBInstanceNotFound")
		}
		if f.dbSnaps[id] != nil {
			return errOut("DBSnapshotAlreadyExists")
		}
		s := &fsnap{db: src.info, data: src.data, created: f.tick(), polls: f.SnapshotPolls}
		f.dbSnaps[id] = s
		st := "available"
		if s.polls > 0 {
			st = "creating"
		}
		if cluster {
			return js(map[string]any{"DBClusterSnapshot": map[string]string{"Status": st}})
		}
		return js(map[string]any{"DBSnapshot": map[string]string{"Status": st}})

	case "describe-db-snapshots", "describe-db-cluster-snapshots":
		cluster := op == "describe-db-cluster-snapshots"
		idKey, listKey, idFlag := "DBSnapshotIdentifier", "DBSnapshots", "db-snapshot-identifier"
		if cluster {
			idKey, listKey, idFlag = "DBClusterSnapshotIdentifier", "DBClusterSnapshots", "db-cluster-snapshot-identifier"
		}
		var list []map[string]any
		if id := flags[idFlag]; id != "" {
			s := f.dbSnaps[id]
			if s == nil || s.db.Cluster != cluster {
				return errOut("DBSnapshotNotFound) when calling the operation: " + id + " not found")
			}
			list = append(list, map[string]any{idKey: id, "Status": s.status(), "SnapshotCreateTime": s.created})
		} else {
			src := inst
			if cluster {
				src = cl
			}
			for id, s := range f.dbSnaps {
				if s.db.ID == src && s.db.Cluster == cluster {
					list = append(list, map[string]any{idKey: id, "Status": s.status(), "SnapshotCreateTime": s.created})
				}
			}
		}
		return js(map[string]any{listKey: list})

	case "restore-db-instance-from-db-snapshot", "restore-db-cluster-from-snapshot":
		cluster := op == "restore-db-cluster-from-snapshot"
		target, snapID, m := inst, flags["db-snapshot-identifier"], f.dbs
		if cluster {
			target, snapID, m = cl, flags["snapshot-identifier"], f.clusters
		}
		if m[target] != nil {
			return errOut("DBInstanceAlreadyExists")
		}
		s := f.dbSnaps[snapID]
		if s == nil {
			return errOut("DBSnapshotNotFound")
		}
		if s.polls > 0 {
			return errOut("InvalidDBSnapshotState) snapshot is not available")
		}
		f.nextID++
		kind := "db"
		if cluster {
			kind = "cluster"
		}
		info := awscli.DBInfo{
			ID: target, Cluster: cluster, Status: "available", Engine: s.db.Engine,
			// Like AWS: same name, same ARN, new resource id.
			Arn:                fmt.Sprintf("arn:aws:rds:us-east-1:%s:%s:%s", f.Account, kind, target),
			ResourceID:         fmt.Sprintf("%s-res-%04d", kind, f.nextID),
			EngineVersion:      flags["engine-version"],
			Class:              flags["db-instance-class"],
			SubnetGroup:        flags["db-subnet-group-name"],
			SecurityGroups:     multi(args, "vpc-security-group-ids"),
			ParameterGroup:     flags["db-parameter-group-name"] + flags["db-cluster-parameter-group-name"],
			OptionGroup:        flags["option-group-name"],
			StorageType:        flags["storage-type"],
			MultiAZ:            flags["multi-az"] == "true",
			Public:             flags["publicly-accessible"] == "true",
			DeletionProtection: flags["deletion-protection"] == "true",
			BackupRetention:    1, // a restored database starts with the default
		}
		if cluster && flags["engine"] != s.db.Engine {
			return errOut("InvalidParameterCombination) engine does not match the snapshot")
		}
		info.Port, _ = strconv.Atoi(flags["port"])
		if t := flags["tags"]; t != "" {
			if err := json.Unmarshal([]byte(t), &info.Tags); err != nil {
				return errOut("InvalidParameterValue) bad tags")
			}
		}
		m[target] = &fdb{info: info, data: s.data, creatingPolls: f.RestorePolls}
		if cluster {
			return js(map[string]any{"DBCluster": map[string]string{"DBClusterArn": info.Arn, "DbClusterResourceId": info.ResourceID}})
		}
		return js(map[string]any{"DBInstance": map[string]string{"DBInstanceArn": info.Arn, "DbiResourceId": info.ResourceID}})

	case "create-db-instance":
		c := f.clusters[cl]
		if c == nil {
			return errOut("DBClusterNotFoundFault")
		}
		info := awscli.DBInfo{ID: inst, ClusterID: cl, Engine: flags["engine"], Class: flags["db-instance-class"], Status: "available",
			Arn: fmt.Sprintf("arn:aws:rds:us-east-1:%s:db:%s", f.Account, inst), ResourceID: "db-res-" + inst}
		f.dbs[inst] = &fdb{info: info}
		c.info.Members = append(c.info.Members, awscli.DBMember{ID: inst, Class: info.Class})
		return js(map[string]any{"DBInstance": map[string]string{"DBInstanceArn": info.Arn}})

	case "modify-db-instance", "modify-db-cluster":
		d := f.dbs[inst]
		if op == "modify-db-cluster" {
			d = f.clusters[cl]
		}
		if d == nil {
			return errOut("DBInstanceNotFound")
		}
		if v := flags["backup-retention-period"]; v != "" {
			d.info.BackupRetention, _ = strconv.Atoi(v)
		}
		return js(map[string]any{})
	}
	return errOut("fake aws: unsupported rds call " + op)
}

func (f *Fake) cacheCall(op string, flags map[string]string, args []string) ([]byte, error) {
	f.rdsInit()
	errOut := func(msg string) ([]byte, error) {
		return nil, &awscli.Error{Args: args, Code: 254, Stderr: "An error occurred (" + msg + ")"}
	}
	js := func(v any) ([]byte, error) { return json.Marshal(v) }
	switch op {
	case "describe-cache-clusters":
		c := f.caches[flags["cache-cluster-id"]]
		if c == nil || c.serverless {
			return errOut("CacheClusterNotFound")
		}
		return js(map[string]any{"CacheClusters": []any{map[string]string{"Engine": c.engine, "ReplicationGroupId": c.group}}})
	case "describe-serverless-caches":
		c := f.caches[flags["serverless-cache-name"]]
		if c == nil || !c.serverless {
			return errOut("ServerlessCacheNotFoundFault")
		}
		return js(map[string]any{"ServerlessCaches": []any{map[string]string{"Engine": c.engine}}})
	}
	return errOut("fake aws: unsupported elasticache call " + op)
}
