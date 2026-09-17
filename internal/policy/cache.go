package policy

import (
	"context"
	"encoding/json"
	"fmt"
	"time"

	"github.com/redis/go-redis/v9"
)

const cacheTTL = 5 * time.Minute

type Cache struct {
	rdb *redis.Client
}

func NewCache(rdb *redis.Client) *Cache { return &Cache{rdb: rdb} }

type permissionDecision struct {
	Allowed bool   `json:"allowed"`
	Reason  string `json:"reason"`
}

func (c *Cache) GetPermission(ctx context.Context, userID, resourceType, resourceID, action string) (allowed bool, found bool) {
	key := permKey(userID, resourceType, resourceID, action)
	val, err := c.rdb.Get(ctx, key).Result()
	if err != nil {
		return false, false
	}
	var d permissionDecision
	if err := json.Unmarshal([]byte(val), &d); err != nil {
		return false, false
	}
	return d.Allowed, true
}

func (c *Cache) SetPermission(ctx context.Context, userID, resourceType, resourceID, action string, allowed bool, reason string) {
	key := permKey(userID, resourceType, resourceID, action)
	d := permissionDecision{Allowed: allowed, Reason: reason}
	b, _ := json.Marshal(d)
	c.rdb.Set(ctx, key, b, cacheTTL)
}

// InvalidateApplication removes all cached decisions for the given application.
// Called when any policy for the application changes.
func (c *Cache) InvalidateApplication(ctx context.Context, applicationID string) error {
	pattern := fmt.Sprintf("perm:*:%s:*", applicationID)
	var cursor uint64
	for {
		keys, nextCursor, err := c.rdb.Scan(ctx, cursor, pattern, 100).Result()
		if err != nil {
			return err
		}
		if len(keys) > 0 {
			c.rdb.Del(ctx, keys...)
		}
		cursor = nextCursor
		if cursor == 0 {
			break
		}
	}
	return nil
}

func permKey(userID, resourceType, resourceID, action string) string {
	return fmt.Sprintf("perm:%s:%s:%s:%s", userID, resourceType, resourceID, action)
}
