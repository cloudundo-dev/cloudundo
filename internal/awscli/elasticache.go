package awscli

import (
	"context"
	"encoding/json"
	"fmt"
)

// CacheInfo is what cloudundo needs to judge an ElastiCache delete.
type CacheInfo struct {
	Engine           string // redis, valkey or memcached
	ReplicationGroup string // set when a cache cluster is one node of a group
}

// DescribeCache returns a cache cluster or serverless cache, or ok=false
// if it does not exist.
func (c Client) DescribeCache(ctx context.Context, id string, serverless bool) (CacheInfo, bool, error) {
	args := []string{"elasticache", "describe-cache-clusters", "--cache-cluster-id", id}
	if serverless {
		args = []string{"elasticache", "describe-serverless-caches", "--serverless-cache-name", id}
	}
	out, err := c.call(ctx, args...)
	if Has(err, "NotFound") {
		return CacheInfo{}, false, nil
	}
	if err != nil {
		return CacheInfo{}, false, err
	}
	var r struct {
		CacheClusters []struct {
			Engine             string
			ReplicationGroupId string
		}
		ServerlessCaches []struct{ Engine string }
	}
	if err := json.Unmarshal(out, &r); err != nil {
		return CacheInfo{}, false, fmt.Errorf("read cache %s: %w", id, err)
	}
	switch {
	case len(r.CacheClusters) > 0:
		return CacheInfo{Engine: r.CacheClusters[0].Engine, ReplicationGroup: r.CacheClusters[0].ReplicationGroupId}, true, nil
	case len(r.ServerlessCaches) > 0:
		return CacheInfo{Engine: r.ServerlessCaches[0].Engine}, true, nil
	}
	return CacheInfo{}, false, nil
}
