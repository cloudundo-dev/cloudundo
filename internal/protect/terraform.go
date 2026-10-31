package protect

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/cloudundo-dev/cloudundo/internal/awscli"
	"github.com/cloudundo-dev/cloudundo/internal/effects"
)

// Planner returns `terraform show -json` of the plan a terraform command
// would carry out, without changing anything.
type Planner func(ctx context.Context, tf effects.Terraform, conn awscli.Conn) ([]byte, error)

// PlanTimeout bounds making the plan; the agent's hook waits at most ten
// minutes for everything.
const PlanTimeout = 4 * time.Minute

// ExecPlanner runs the real terraform (or tofu). For an apply of a saved
// plan it only reads that plan; otherwise it makes a plan into a
// temporary file with -lock=false, which reads state but never writes it.
func ExecPlanner(ctx context.Context, tf effects.Terraform, conn awscli.Conn) ([]byte, error) {
	ctx, cancel := context.WithTimeout(ctx, PlanTimeout)
	defer cancel()
	run := func(args ...string) ([]byte, error) {
		cmd := exec.CommandContext(ctx, tf.Bin, args...)
		cmd.Dir = tf.Dir
		cmd.Env = append(append(os.Environ(), conn.Environ()...), "TF_IN_AUTOMATION=1", "TF_INPUT=0")
		cmd.WaitDelay = 5 * time.Second
		var out, errb bytes.Buffer
		cmd.Stdout, cmd.Stderr = &out, &errb
		if err := cmd.Run(); err != nil {
			if ctx.Err() == context.DeadlineExceeded {
				return nil, fmt.Errorf("%s %s took more than %s", tf.Bin, args[0], PlanTimeout)
			}
			msg := strings.TrimSpace(errb.String())
			if len(msg) > 600 {
				msg = msg[:600] + "..."
			}
			return nil, fmt.Errorf("%s %s failed: %v: %s", tf.Bin, args[0], err, msg)
		}
		return out.Bytes(), nil
	}
	if tf.PlanFile != "" {
		return run("show", "-json", "-no-color", tf.PlanFile)
	}
	tmp, err := os.MkdirTemp("", "cloudundo-plan-")
	if err != nil {
		return nil, err
	}
	defer os.RemoveAll(tmp)
	planFile := filepath.Join(tmp, "plan")
	args := []string{"plan", "-input=false", "-lock=false", "-no-color", "-out=" + planFile}
	if tf.Destroy {
		args = append(args, "-destroy")
	}
	args = append(args, tf.PlanArgs...)
	if _, err := run(args...); err != nil {
		return nil, err
	}
	return run("show", "-json", "-no-color", planFile)
}

type planJSON struct {
	ResourceChanges []struct {
		Address string `json:"address"`
		Mode    string `json:"mode"`
		Type    string `json:"type"`
		Change  struct {
			Actions []string       `json:"actions"`
			Before  map[string]any `json:"before"`
		} `json:"change"`
	} `json:"resource_changes"`
}

// fromPlan turns the resources a plan deletes or replaces into the
// effects cloudundo knows how to protect. Infrastructure without data
// (networks, load balancers, IAM) is only counted.
func fromPlan(body []byte, tf effects.Effect) (effs []effects.Effect, others []string, err error) {
	var p planJSON
	if err := json.Unmarshal(body, &p); err != nil {
		return nil, nil, fmt.Errorf("could not read the terraform plan: %w", err)
	}
	deletedBuckets := map[string]bool{}
	for _, rc := range p.ResourceChanges {
		if rc.Type == "aws_s3_bucket" && deletes(rc.Change.Actions) {
			deletedBuckets[str(rc.Change.Before, "bucket")] = true
		}
	}
	for _, rc := range p.ResourceChanges {
		if rc.Mode == "data" || !deletes(rc.Change.Actions) {
			continue
		}
		b := rc.Change.Before
		e := effects.Effect{Service: serviceOf(rc.Type), Command: tf.Command, Via: rc.Address, Conn: tf.Conn}
		if r := regionOf(b); r != "" {
			e.Conn.Region = r
		}
		switch rc.Type {
		case "aws_db_instance", "aws_rds_cluster", "aws_rds_cluster_instance":
			e.Kind = effects.DeleteDatabase
			e.Cluster = rc.Type == "aws_rds_cluster"
			e.Database = str(b, "identifier")
			if e.Cluster {
				e.Database = str(b, "cluster_identifier")
			}
			if boolean(b, "skip_final_snapshot") {
				e.SkipFinalSnapshot = true
			} else {
				e.FinalSnapshot = str(b, "final_snapshot_identifier")
			}
			if rc.Type == "aws_rds_cluster_instance" {
				e.SkipFinalSnapshot = true // instances of a cluster keep no data of their own
			}
		case "aws_s3_bucket":
			e.Kind = effects.DeleteBucket
			e.Bucket = str(b, "bucket")
			e.Force = boolean(b, "force_destroy")
		case "aws_s3_object", "aws_s3_bucket_object":
			e.Kind = effects.DeleteObjects
			e.Bucket, e.Key = str(b, "bucket"), str(b, "key")
		case "aws_s3_bucket_versioning":
			if deletedBuckets[str(b, "bucket")] {
				continue // the bucket goes too, and is copied first
			}
			e.Kind = effects.WeakenProtection
			e.Bucket = str(b, "bucket")
			e.Detail = "suspends versioning (" + rc.Address + ")"
		case "aws_dynamodb_table":
			e.Kind = effects.DeleteTable
			e.Table = str(b, "name")
		case "aws_dynamodb_table_item":
			e.Kind = effects.DeleteItems
			e.Table = str(b, "table_name")
			e.FullItems = true
			item := str(b, "item")
			var v map[string]any
			if json.Unmarshal([]byte(item), &v) != nil {
				e.Uncertain = true
			} else {
				compact, _ := json.Marshal(v)
				e.Items = []string{string(compact)}
			}
		case "aws_elasticache_cluster":
			e.Kind, e.CacheKind = effects.DeleteCache, effects.CacheCluster
			e.Cache, e.FinalSnapshot = str(b, "cluster_id"), str(b, "final_snapshot_identifier")
		case "aws_elasticache_replication_group":
			e.Kind, e.CacheKind = effects.DeleteCache, effects.CacheReplicationGroup
			e.Cache, e.FinalSnapshot = str(b, "replication_group_id"), str(b, "final_snapshot_identifier")
		case "aws_elasticache_serverless_cache":
			e.Kind, e.CacheKind = effects.DeleteCache, effects.CacheServerless
			e.Cache = str(b, "name")
		case "aws_db_snapshot", "aws_db_cluster_snapshot":
			e.Kind = effects.WeakenProtection
			e.Database = str(b, "db_snapshot_identifier") + str(b, "db_cluster_snapshot_identifier")
			e.Detail = "deletes a database snapshot (" + rc.Address + ")"
		default:
			if !unprotectedTypes[rc.Type] {
				others = append(others, rc.Address)
				continue
			}
			e.Kind = effects.UnprotectedData
			e.Detail = rc.Type
			e.Database = rc.Address // named, so it is not taken for an unknown target
		}
		if e.Bucket == "" && e.Table == "" && e.Database == "" && e.Cache == "" {
			e.Uncertain = true
		}
		effs = append(effs, e)
	}
	sort.Strings(others)
	return effs, others, nil
}

// unprotectedTypes hold data that cloudundo cannot back up yet; a plan
// that deletes one is blocked rather than let through unprotected.
var unprotectedTypes = map[string]bool{
	"aws_ebs_volume": true, "aws_efs_file_system": true, "aws_fsx_lustre_file_system": true,
	"aws_fsx_windows_file_system": true, "aws_fsx_ontap_file_system": true, "aws_fsx_openzfs_file_system": true,
	"aws_redshift_cluster": true, "aws_redshiftserverless_namespace": true,
	"aws_docdb_cluster": true, "aws_neptune_cluster": true, "aws_memorydb_cluster": true,
	"aws_opensearch_domain": true, "aws_elasticsearch_domain": true,
	"aws_keyspaces_table": true, "aws_timestreamwrite_table": true, "aws_qldb_ledger": true,
	"aws_kinesis_stream": true, "aws_glacier_vault": true, "aws_backup_vault": true,
	"aws_ecr_repository": true, "aws_kms_key": true, "aws_secretsmanager_secret": true,
	"aws_lightsail_database": true, "aws_lightsail_disk": true,
}

func deletes(actions []string) bool {
	for _, a := range actions {
		if a == "delete" {
			return true
		}
	}
	return false
}

func serviceOf(t string) string {
	switch {
	case strings.HasPrefix(t, "aws_s3_"):
		return "s3"
	case strings.HasPrefix(t, "aws_dynamodb_"):
		return "dynamodb"
	case strings.HasPrefix(t, "aws_db_"), strings.HasPrefix(t, "aws_rds_"):
		return "rds"
	case strings.HasPrefix(t, "aws_elasticache_"):
		return "elasticache"
	}
	return "terraform"
}

func str(m map[string]any, k string) string {
	s, _ := m[k].(string)
	return s
}

func boolean(m map[string]any, k string) bool {
	b, _ := m[k].(bool)
	return b
}

// regionOf reads the region from a resource's ARN or region attribute.
func regionOf(m map[string]any) string {
	if parts := strings.Split(str(m, "arn"), ":"); len(parts) > 4 && parts[3] != "" {
		return parts[3]
	}
	return str(m, "region")
}

// accountOf reads the account from a resource's ARN.
func accountOf(m map[string]any) string {
	if parts := strings.Split(str(m, "arn"), ":"); len(parts) > 4 {
		return parts[4]
	}
	return ""
}

var errOtherAccount = errors.New("other account")

// checkAccounts refuses a plan whose resources live in an account other
// than the one cloudundo's credentials reach (a provider with its own
// profile or role): protecting the wrong account would protect nothing.
func checkAccounts(ctx context.Context, body []byte, c awscli.Client) error {
	var p planJSON
	if err := json.Unmarshal(body, &p); err != nil {
		return err
	}
	want := map[string]string{}
	for _, rc := range p.ResourceChanges {
		if rc.Mode == "data" || !deletes(rc.Change.Actions) || serviceOf(rc.Type) == "terraform" {
			continue
		}
		if a := accountOf(rc.Change.Before); a != "" {
			want[a] = rc.Address
		}
	}
	if len(want) == 0 {
		return nil
	}
	have, err := c.AccountID(ctx)
	if err != nil {
		return err
	}
	for acct, addr := range want {
		if acct != have {
			return fmt.Errorf("%w: %s is in AWS account %s, but your current AWS credentials reach %s; set AWS_PROFILE to that account so cloudundo can protect it", errOtherAccount, addr, acct, have)
		}
	}
	return nil
}

// hasState reports whether a plan's prior state holds any managed
// resource, in the root module or a child module.
func hasState(body []byte) bool {
	var p struct {
		PriorState struct {
			Values struct {
				RootModule stateModule `json:"root_module"`
			} `json:"values"`
		} `json:"prior_state"`
	}
	if json.Unmarshal(body, &p) != nil {
		return false
	}
	return p.PriorState.Values.RootModule.managed()
}

type stateModule struct {
	Resources []struct {
		Mode string `json:"mode"`
	} `json:"resources"`
	ChildModules []stateModule `json:"child_modules"`
}

func (m stateModule) managed() bool {
	for _, r := range m.Resources {
		if r.Mode == "managed" {
			return true
		}
	}
	for _, c := range m.ChildModules {
		if c.managed() {
			return true
		}
	}
	return false
}

// tfDir is the absolute folder a plan is read in.
func tfDir(dir string) string {
	if a, err := filepath.Abs(dir); err == nil {
		return a
	}
	return dir
}
