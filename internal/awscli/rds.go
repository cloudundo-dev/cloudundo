package awscli

import (
	"context"
	"encoding/json"
	"fmt"
	"strconv"
	"time"
)

// DBInfo is what cloudundo records about an RDS instance or Aurora
// cluster, enough to restore it with the same settings.
type DBInfo struct {
	ID  string `json:"id"`
	Arn string `json:"arn"`
	// ResourceID is unique to this database; one restored or re-created
	// under the same name gets a new one (the ARN stays the same).
	ResourceID     string   `json:"resource_id,omitempty"`
	Status         string   `json:"status"`
	Cluster        bool     `json:"cluster,omitempty"`
	Engine         string   `json:"engine"`
	EngineVersion  string   `json:"engine_version,omitempty"`
	Class          string   `json:"class,omitempty"`
	SubnetGroup    string   `json:"subnet_group,omitempty"`
	SecurityGroups []string `json:"security_groups,omitempty"`
	ParameterGroup string   `json:"parameter_group,omitempty"`
	OptionGroup    string   `json:"option_group,omitempty"`
	Port           int      `json:"port,omitempty"`
	MultiAZ        bool     `json:"multi_az,omitempty"`
	Public         bool     `json:"public,omitempty"`
	StorageType    string   `json:"storage_type,omitempty"`
	// DeletionProtection was on before the delete (AWS refuses deletes
	// while it is on, so it was turned off first, by someone).
	DeletionProtection bool `json:"deletion_protection,omitempty"`
	BackupRetention    int  `json:"backup_retention,omitempty"`
	// ClusterID is the Aurora cluster an instance belongs to.
	ClusterID string `json:"cluster_id,omitempty"`
	// Members are a cluster's instances.
	Members []DBMember `json:"members,omitempty"`
	Tags    []Tag      `json:"tags,omitempty"`
}

// DBMember is one instance of an Aurora cluster.
type DBMember struct {
	ID     string `json:"id"`
	Class  string `json:"class,omitempty"`
	Writer bool   `json:"writer,omitempty"`
}

// DBSnapshot is a manual or final RDS snapshot.
type DBSnapshot struct {
	ID      string
	Status  string
	Created time.Time
}

type rdsTag struct{ Key, Value string }

func tagsOf(in []rdsTag) []Tag {
	var out []Tag
	for _, t := range in {
		out = append(out, Tag{Key: t.Key, Value: t.Value})
	}
	return out
}

// DescribeDB returns an RDS instance (or cluster), or ok=false if it
// does not exist.
func (c Client) DescribeDB(ctx context.Context, id string, cluster bool) (DBInfo, bool, error) {
	if cluster {
		return c.describeCluster(ctx, id)
	}
	out, err := c.call(ctx, "rds", "describe-db-instances", "--db-instance-identifier", id)
	if Has(err, "DBInstanceNotFound") {
		return DBInfo{}, false, nil
	}
	if err != nil {
		return DBInfo{}, false, err
	}
	var r struct {
		DBInstances []struct {
			DBInstanceIdentifier, DBInstanceArn, DBInstanceStatus string
			DbiResourceId                                         string
			DBInstanceClass, Engine, EngineVersion, StorageType   string
			DBSubnetGroup                                         *struct{ DBSubnetGroupName string }
			VpcSecurityGroups                                     []struct{ VpcSecurityGroupId string }
			DBParameterGroups                                     []struct{ DBParameterGroupName string }
			OptionGroupMemberships                                []struct{ OptionGroupName string }
			Endpoint                                              *struct{ Port int }
			DbInstancePort                                        int
			MultiAZ, PubliclyAccessible, DeletionProtection       bool
			BackupRetentionPeriod                                 int
			DBClusterIdentifier                                   string
			TagList                                               []rdsTag
		}
	}
	if err := json.Unmarshal(out, &r); err != nil {
		return DBInfo{}, false, fmt.Errorf("read database %s: %w", id, err)
	}
	if len(r.DBInstances) == 0 {
		return DBInfo{}, false, nil
	}
	d := r.DBInstances[0]
	info := DBInfo{ID: d.DBInstanceIdentifier, Arn: d.DBInstanceArn, ResourceID: d.DbiResourceId, Status: d.DBInstanceStatus,
		Engine: d.Engine, EngineVersion: d.EngineVersion, Class: d.DBInstanceClass, StorageType: d.StorageType,
		MultiAZ: d.MultiAZ, Public: d.PubliclyAccessible, DeletionProtection: d.DeletionProtection,
		BackupRetention: d.BackupRetentionPeriod, ClusterID: d.DBClusterIdentifier, Tags: tagsOf(d.TagList)}
	if d.DBSubnetGroup != nil {
		info.SubnetGroup = d.DBSubnetGroup.DBSubnetGroupName
	}
	for _, g := range d.VpcSecurityGroups {
		info.SecurityGroups = append(info.SecurityGroups, g.VpcSecurityGroupId)
	}
	if len(d.DBParameterGroups) > 0 {
		info.ParameterGroup = d.DBParameterGroups[0].DBParameterGroupName
	}
	if len(d.OptionGroupMemberships) > 0 {
		info.OptionGroup = d.OptionGroupMemberships[0].OptionGroupName
	}
	if d.Endpoint != nil {
		info.Port = d.Endpoint.Port
	} else {
		info.Port = d.DbInstancePort
	}
	return info, true, nil
}

func (c Client) describeCluster(ctx context.Context, id string) (DBInfo, bool, error) {
	out, err := c.call(ctx, "rds", "describe-db-clusters", "--db-cluster-identifier", id)
	if Has(err, "DBClusterNotFound") {
		return DBInfo{}, false, nil
	}
	if err != nil {
		return DBInfo{}, false, err
	}
	var r struct {
		DBClusters []struct {
			DBClusterIdentifier, DBClusterArn, Status, Engine, EngineVersion string
			DbClusterResourceId                                              string
			DBSubnetGroup, DBClusterParameterGroup                           string
			VpcSecurityGroups                                                []struct{ VpcSecurityGroupId string }
			Port                                                             int
			DeletionProtection                                               bool
			BackupRetentionPeriod                                            int
			DBClusterMembers                                                 []struct {
				DBInstanceIdentifier string
				IsClusterWriter      bool
			}
			TagList []rdsTag
		}
	}
	if err := json.Unmarshal(out, &r); err != nil {
		return DBInfo{}, false, fmt.Errorf("read cluster %s: %w", id, err)
	}
	if len(r.DBClusters) == 0 {
		return DBInfo{}, false, nil
	}
	d := r.DBClusters[0]
	info := DBInfo{ID: d.DBClusterIdentifier, Arn: d.DBClusterArn, ResourceID: d.DbClusterResourceId, Status: d.Status, Cluster: true,
		Engine: d.Engine, EngineVersion: d.EngineVersion, SubnetGroup: d.DBSubnetGroup,
		ParameterGroup: d.DBClusterParameterGroup, Port: d.Port, DeletionProtection: d.DeletionProtection,
		BackupRetention: d.BackupRetentionPeriod, Tags: tagsOf(d.TagList)}
	for _, g := range d.VpcSecurityGroups {
		info.SecurityGroups = append(info.SecurityGroups, g.VpcSecurityGroupId)
	}
	for _, m := range d.DBClusterMembers {
		info.Members = append(info.Members, DBMember{ID: m.DBInstanceIdentifier, Writer: m.IsClusterWriter})
	}
	return info, true, nil
}

// CreateDBSnapshot starts a manual snapshot and returns its status.
func (c Client) CreateDBSnapshot(ctx context.Context, id, snapshot string, cluster bool) (string, error) {
	if cluster {
		out, err := c.call(ctx, "rds", "create-db-cluster-snapshot", "--db-cluster-identifier", id,
			"--db-cluster-snapshot-identifier", snapshot)
		if err != nil {
			return "", err
		}
		var r struct{ DBClusterSnapshot struct{ Status string } }
		if err := json.Unmarshal(out, &r); err != nil {
			return "", fmt.Errorf("read snapshot %s: %w", snapshot, err)
		}
		return r.DBClusterSnapshot.Status, nil
	}
	out, err := c.call(ctx, "rds", "create-db-snapshot", "--db-instance-identifier", id,
		"--db-snapshot-identifier", snapshot)
	if err != nil {
		return "", err
	}
	var r struct{ DBSnapshot struct{ Status string } }
	if err := json.Unmarshal(out, &r); err != nil {
		return "", fmt.Errorf("read snapshot %s: %w", snapshot, err)
	}
	return r.DBSnapshot.Status, nil
}

// DBSnapshots lists the manual snapshots of a database, or the one named
// snapshot when snapshot is set. A missing snapshot is an empty list.
func (c Client) DBSnapshots(ctx context.Context, id, snapshot string, cluster bool) ([]DBSnapshot, error) {
	var args []string
	if cluster {
		args = []string{"rds", "describe-db-cluster-snapshots"}
		if snapshot != "" {
			args = append(args, "--db-cluster-snapshot-identifier", snapshot)
		} else {
			args = append(args, "--db-cluster-identifier", id, "--snapshot-type", "manual")
		}
	} else {
		args = []string{"rds", "describe-db-snapshots"}
		if snapshot != "" {
			args = append(args, "--db-snapshot-identifier", snapshot)
		} else {
			args = append(args, "--db-instance-identifier", id, "--snapshot-type", "manual")
		}
	}
	out, err := c.call(ctx, args...)
	if Has(err, "SnapshotNotFound") {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var r struct {
		DBSnapshots []struct {
			DBSnapshotIdentifier, Status string
			SnapshotCreateTime           time.Time
		}
		DBClusterSnapshots []struct {
			DBClusterSnapshotIdentifier, Status string
			SnapshotCreateTime                  time.Time
		}
	}
	if err := json.Unmarshal(out, &r); err != nil {
		return nil, fmt.Errorf("read snapshots of %s: %w", id, err)
	}
	var list []DBSnapshot
	for _, s := range r.DBSnapshots {
		list = append(list, DBSnapshot{ID: s.DBSnapshotIdentifier, Status: s.Status, Created: s.SnapshotCreateTime})
	}
	for _, s := range r.DBClusterSnapshots {
		list = append(list, DBSnapshot{ID: s.DBClusterSnapshotIdentifier, Status: s.Status, Created: s.SnapshotCreateTime})
	}
	return list, nil
}

// RestoreDB recreates a database from a snapshot with the recorded
// settings, and returns the new database's resource id.
func (c Client) RestoreDB(ctx context.Context, d DBInfo, snapshot string) (string, error) {
	var args []string
	if d.Cluster {
		args = []string{"rds", "restore-db-cluster-from-snapshot", "--db-cluster-identifier", d.ID,
			"--snapshot-identifier", snapshot, "--engine", d.Engine}
		if d.EngineVersion != "" {
			args = append(args, "--engine-version", d.EngineVersion)
		}
		if d.ParameterGroup != "" {
			args = append(args, "--db-cluster-parameter-group-name", d.ParameterGroup)
		}
	} else {
		args = []string{"rds", "restore-db-instance-from-db-snapshot", "--db-instance-identifier", d.ID,
			"--db-snapshot-identifier", snapshot}
		if d.Class != "" {
			args = append(args, "--db-instance-class", d.Class)
		}
		if d.ParameterGroup != "" {
			args = append(args, "--db-parameter-group-name", d.ParameterGroup)
		}
		if d.OptionGroup != "" {
			args = append(args, "--option-group-name", d.OptionGroup)
		}
		if d.StorageType != "" {
			args = append(args, "--storage-type", d.StorageType)
		}
		if d.MultiAZ {
			args = append(args, "--multi-az")
		} else {
			args = append(args, "--no-multi-az")
		}
		if d.Public {
			args = append(args, "--publicly-accessible")
		} else {
			args = append(args, "--no-publicly-accessible")
		}
	}
	if d.SubnetGroup != "" {
		args = append(args, "--db-subnet-group-name", d.SubnetGroup)
	}
	if len(d.SecurityGroups) > 0 {
		args = append(args, "--vpc-security-group-ids")
		args = append(args, d.SecurityGroups...)
	}
	if d.Port != 0 {
		args = append(args, "--port", strconv.Itoa(d.Port))
	}
	if d.DeletionProtection {
		args = append(args, "--deletion-protection")
	}
	if len(d.Tags) > 0 {
		b, _ := json.Marshal(d.Tags)
		args = append(args, "--tags", string(b))
	}
	out, err := c.call(ctx, args...)
	if err != nil {
		return "", err
	}
	var r struct {
		DBInstance struct{ DbiResourceId string }
		DBCluster  struct{ DbClusterResourceId string }
	}
	if err := json.Unmarshal(out, &r); err != nil {
		return "", fmt.Errorf("read restored database %s: %w", d.ID, err)
	}
	if d.Cluster {
		return r.DBCluster.DbClusterResourceId, nil
	}
	return r.DBInstance.DbiResourceId, nil
}

// CreateClusterInstance adds an instance to an Aurora cluster.
func (c Client) CreateClusterInstance(ctx context.Context, cluster DBInfo, m DBMember) error {
	_, err := c.call(ctx, "rds", "create-db-instance", "--db-instance-identifier", m.ID,
		"--db-cluster-identifier", cluster.ID, "--engine", cluster.Engine, "--db-instance-class", m.Class)
	return err
}

// SetBackupRetention sets how many days of automated backups are kept.
func (c Client) SetBackupRetention(ctx context.Context, d DBInfo, days int) error {
	op, flag := "modify-db-instance", "--db-instance-identifier"
	if d.Cluster {
		op, flag = "modify-db-cluster", "--db-cluster-identifier"
	}
	_, err := c.call(ctx, "rds", op, flag, d.ID, "--backup-retention-period", strconv.Itoa(days), "--apply-immediately")
	return err
}
