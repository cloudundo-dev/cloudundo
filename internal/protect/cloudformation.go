package protect

import (
	"context"
	"fmt"
	"sort"
	"strings"

	"github.com/cloudundo/cloudundo/internal/awscli"
	"github.com/cloudundo/cloudundo/internal/effects"
)

// maxNested bounds how deep nested stacks are followed.
const maxNested = 5

// stackPlan reads what a CloudFormation command would delete: every
// resource of a deleted stack, or the removals and replacements of a
// change set (for a deploy, one made with --no-execute-changeset and
// deleted again). Resources the template keeps (DeletionPolicy: Retain)
// are left out.
func (r *Result) stackPlan(ctx context.Context, e effects.Effect, clientFor ClientFor) ([]effects.Effect, bool) {
	c := clientFor(e.Conn)
	st := e.Stack
	p := stackPlanner{r: r, c: c, e: e}
	var ok bool
	switch st.Op {
	case "delete-stack":
		ok = p.deleteStack(ctx, st.Name, st.Retain, 0)
	case "execute-change-set":
		ok = p.changeSet(ctx, st.ChangeSet, st.Name, 0)
	case "deploy":
		ok = p.deploy(ctx, st)
	default:
		r.Blocked = append(r.Blocked, fmt.Sprintf("cloudundo cannot read %q yet.", e.Command))
		return nil, false
	}
	if !ok {
		return nil, false
	}
	if len(p.kept) > 0 {
		sort.Strings(p.kept)
		r.Notes = append(r.Notes, fmt.Sprintf("The template keeps %s (DeletionPolicy or UpdateReplacePolicy Retain), so they are not backed up.", shortList(p.kept, 4)))
	}
	if len(p.others) > 0 {
		sort.Strings(p.others)
		r.Notes = append(r.Notes, fmt.Sprintf("%q also deletes %d resource(s) that cloudundo does not restore (%s): infrastructure such as functions, networks and roles, not stored data. Deploy the stack again for those.",
			e.Command, len(p.others), shortList(p.others, 4)))
	}
	if len(p.derived) == 0 {
		r.Notes = append(r.Notes, fmt.Sprintf("%q deletes no data cloudundo knows (S3, DynamoDB, RDS, ElastiCache).", e.Command))
	}
	return p.derived, true
}

type stackPlanner struct {
	r       *Result
	c       awscli.Client
	e       effects.Effect
	derived []effects.Effect
	others  []string
	kept    []string
}

func (p *stackPlanner) block(format string, a ...any) bool {
	p.r.Blocked = append(p.r.Blocked, fmt.Sprintf(format, a...))
	return false
}

// policies reads the stack's template. With no readable template (YAML),
// every resource is taken as deleted: protecting one that is kept costs
// a backup; skipping one that is deleted would lose data.
func (p *stackPlanner) policies(ctx context.Context, stack string) (map[string]awscli.StackPolicy, bool) {
	pol, ok, err := p.c.StackPolicies(ctx, stack)
	if err != nil {
		p.block("could not read the template of stack %s: %v", stack, err)
		return nil, false
	}
	if !ok {
		return map[string]awscli.StackPolicy{}, true
	}
	return pol, true
}

func (p *stackPlanner) deleteStack(ctx context.Context, stack string, retain []string, depth int) bool {
	if depth > maxNested {
		return p.block("stack %s nests more than %d stacks deep; cloudundo cannot follow it", stack, maxNested)
	}
	res, exists, err := p.c.StackResources(ctx, stack)
	if err != nil {
		return p.block("could not list the resources of stack %s: %v", stack, err)
	}
	if !exists {
		p.r.Notes = append(p.r.Notes, fmt.Sprintf("Stack %s does not exist, so the command will fail on its own.", stack))
		return true
	}
	pol, ok := p.policies(ctx, stack)
	if !ok {
		return false
	}
	keep := map[string]bool{}
	for _, id := range retain {
		keep[id] = true
	}
	for _, rs := range res {
		if rs.Physical == "" || rs.Status == "DELETE_COMPLETE" {
			continue // never created, or already gone
		}
		if keep[rs.Logical] || retained(pol[rs.Logical].Deletion) {
			p.kept = append(p.kept, stack+"/"+rs.Logical)
			continue
		}
		if rs.Type == "AWS::CloudFormation::Stack" {
			if !p.deleteStack(ctx, rs.Physical, nil, depth+1) {
				return false
			}
			continue
		}
		p.add(stack, rs.Logical, rs.Type, rs.Physical, pol[rs.Logical].Deletion)
	}
	return true
}

func (p *stackPlanner) changeSet(ctx context.Context, name, stack string, depth int) bool {
	if depth > maxNested {
		return p.block("change set %s nests more than %d stacks deep; cloudundo cannot follow it", name, maxNested)
	}
	cs, err := p.c.ChangeSet(ctx, name, stack)
	if err != nil {
		return p.block("could not read change set %s: %v", name, err)
	}
	if cs.ExecutionStatus != "" && cs.ExecutionStatus != "AVAILABLE" {
		p.r.Notes = append(p.r.Notes, fmt.Sprintf("Change set %s is %s, not ready to run, so the command will fail on its own.", name, cs.ExecutionStatus))
		return true
	}
	if cs.More {
		return p.block("change set %s has more changes than cloudundo reads at once; run it yourself after checking it", name)
	}
	stackRef := cs.StackID
	if stackRef == "" {
		stackRef = cs.StackName
	}
	var pol map[string]awscli.StackPolicy
	for _, ch := range cs.Changes {
		replaced := ch.Action == "Modify" && (ch.Replacement == "True" || ch.Replacement == "Conditional")
		if ch.Action != "Remove" && !replaced && !(ch.Type == "AWS::CloudFormation::Stack" && ch.Action == "Modify") {
			continue
		}
		if ch.Physical == "" {
			continue // a resource that was never created
		}
		if pol == nil {
			var ok bool
			if pol, ok = p.policies(ctx, stackRef); !ok {
				return false
			}
		}
		policy := pol[ch.Logical].Deletion
		if replaced {
			policy = pol[ch.Logical].UpdateReplace
		}
		if retained(policy) {
			p.kept = append(p.kept, cs.StackName+"/"+ch.Logical)
			continue
		}
		if ch.Type == "AWS::CloudFormation::Stack" {
			switch {
			case ch.Action == "Remove" || replaced:
				if !p.deleteStack(ctx, ch.Physical, nil, depth+1) {
					return false
				}
			case ch.ChangeSetID != "":
				if !p.changeSet(ctx, ch.ChangeSetID, "", depth+1) {
					return false
				}
			default:
				return p.block("change set %s changes nested stack %s without showing its changes; make the change set with --include-nested-stacks, or run it yourself", name, ch.Logical)
			}
			continue
		}
		p.add(cs.StackName, ch.Logical, ch.Type, ch.Physical, policy)
	}
	return true
}

func (p *stackPlanner) deploy(ctx context.Context, st *effects.Stack) bool {
	status, exists, err := p.c.StackStatus(ctx, st.Name)
	if err != nil {
		return p.block("could not check stack %s: %v", st.Name, err)
	}
	if !exists || status == "REVIEW_IN_PROGRESS" {
		p.r.Notes = append(p.r.Notes, fmt.Sprintf("Stack %s does not exist yet, so the deploy only creates resources.", st.Name))
		return true
	}
	arn, none, err := p.c.DeployChangeSet(ctx, st.DeployArgs)
	if err != nil {
		return p.block("could not see what deploying stack %s would change: %v", st.Name, err)
	}
	if none {
		p.r.Notes = append(p.r.Notes, fmt.Sprintf("Deploying stack %s changes nothing.", st.Name))
		return true
	}
	ok := p.changeSet(ctx, arn, "", 0)
	if err := p.c.DeleteChangeSet(ctx, arn); err != nil {
		p.r.Notes = append(p.r.Notes, fmt.Sprintf("Could not delete the change set cloudundo made to read the deploy (%s); delete it yourself.", arn))
	}
	return ok
}

// retained says a policy keeps the resource and its data.
func retained(policy string) bool {
	return policy == "Retain" || policy == "RetainExceptOnCreate"
}

// cfnUnprotected hold data cloudundo cannot back up yet; deleting one
// is blocked unless the template snapshots it (snapshotable).
var cfnUnprotected = map[string]bool{
	"AWS::EC2::Volume": true, "AWS::EFS::FileSystem": true, "AWS::FSx::FileSystem": true,
	"AWS::Redshift::Cluster": true, "AWS::RedshiftServerless::Namespace": true,
	"AWS::DocDB::DBCluster": true, "AWS::Neptune::DBCluster": true, "AWS::MemoryDB::Cluster": true,
	"AWS::OpenSearchService::Domain": true, "AWS::Elasticsearch::Domain": true,
	"AWS::Cassandra::Table": true, "AWS::Timestream::Table": true, "AWS::QLDB::Ledger": true,
	"AWS::Kinesis::Stream": true, "AWS::Backup::BackupVault": true, "AWS::ECR::Repository": true,
	"AWS::KMS::Key": true, "AWS::SecretsManager::Secret": true,
	"AWS::Lightsail::Database": true, "AWS::Lightsail::Disk": true,
}

// add turns one deleted resource into the effect cloudundo protects.
func (p *stackPlanner) add(stack, logical, typ, physical, policy string) {
	via := stack + "/" + logical + " (" + typ + ")"
	e := effects.Effect{Command: p.e.Command, Via: via, Conn: p.e.Conn}
	switch typ {
	case "AWS::S3::Bucket":
		e.Kind, e.Service, e.Bucket, e.Force = effects.DeleteBucket, "s3", physical, true
	case "AWS::DynamoDB::Table", "AWS::DynamoDB::GlobalTable":
		e.Kind, e.Service, e.Table = effects.DeleteTable, "dynamodb", physical
	case "AWS::RDS::DBInstance", "AWS::RDS::DBCluster":
		e.Kind, e.Service, e.Database = effects.DeleteDatabase, "rds", physical
		e.Cluster = typ == "AWS::RDS::DBCluster"
		e.SkipFinalSnapshot = true // cloudundo takes its own, which undo knows by name
	case "AWS::ElastiCache::CacheCluster", "AWS::ElastiCache::ReplicationGroup", "AWS::ElastiCache::ServerlessCache":
		e.Kind, e.Service, e.Cache = effects.DeleteCache, "elasticache", physical
		e.CacheKind = map[string]string{
			"AWS::ElastiCache::CacheCluster":     effects.CacheCluster,
			"AWS::ElastiCache::ReplicationGroup": effects.CacheReplicationGroup,
			"AWS::ElastiCache::ServerlessCache":  effects.CacheServerless,
		}[typ]
		if policy == "Snapshot" {
			e.FinalSnapshot = "(DeletionPolicy: Snapshot)"
		}
	default:
		if !cfnUnprotected[typ] {
			p.others = append(p.others, stack+"/"+logical)
			return
		}
		if policy == "Snapshot" {
			p.r.Notes = append(p.r.Notes, fmt.Sprintf("CloudFormation snapshots %s before deleting it (DeletionPolicy: Snapshot); cloudundo does not restore that type.", via))
			return
		}
		e.Kind, e.Service, e.Detail = effects.UnprotectedData, "cloudformation", typ
		e.Database = logical // named, so it is not taken for an unknown target
		e.Via = via
	}
	if strings.TrimSpace(physical) == "" {
		e.Uncertain = true
	}
	p.derived = append(p.derived, e)
}
