package frontend

import (
	"context"
	"fmt"

	"github.com/redis/go-redis/v9"
)

// Classification mirrors the redis-quota gate's reserved/overflow outcome.
type Classification string

const (
	ClassificationReserved Classification = "reserved"
	ClassificationOverflow Classification = "overflow"
)

// quotaClassifier classifies direct-mode requests against the same Redis
// counters and key scheme as the AP's redis-quota gate (concurrency mode,
// classifying semantics: over-quota is labeled overflow, never blocked).
// Queued modes are classified by the AP's gates at dequeue, not here.
type quotaClassifier struct {
	rdb *redis.Client
	cfg QuotaConfig
}

// acquireScript matches the redis-quota gate's atomic check-and-increment,
// including the TTL refresh on every acquire so counters cannot expire while
// requests are in flight.
var acquireScript = redis.NewScript(`
local current = redis.call("GET", KEYS[1])
if current and tonumber(current) >= tonumber(ARGV[1]) then
	return 0
end
redis.call("INCR", KEYS[1])
redis.call("EXPIRE", KEYS[1], ARGV[2])
return 1
`)

var releaseScript = redis.NewScript(`
local current = redis.call("GET", KEYS[1])
if current and tonumber(current) > 0 then
	local remaining = redis.call("DECR", KEYS[1])
	if remaining > 0 then
		redis.call("EXPIRE", KEYS[1], ARGV[1])
	end
end
`)

// classify returns the tenant's classification and a release func that must
// be called when the request completes (reserved acquisitions only). Redis
// errors fail open: the request is classified reserved with a no-op release,
// so a Redis outage never blocks live traffic (quota briefly unenforced).
func (q *quotaClassifier) classify(ctx context.Context, tenant string) (Classification, func(), error) {
	noop := func() {}
	limit, ok := q.cfg.Limits[tenant]
	if !ok || limit <= 0 {
		return ClassificationReserved, noop, nil
	}

	key := fmt.Sprintf("%s%s:%s", q.cfg.Prefix, q.cfg.Attribute, tenant)
	res, err := acquireScript.Run(ctx, q.rdb, []string{key}, limit, q.cfg.WindowSeconds).Result()
	if err != nil {
		return ClassificationReserved, noop, fmt.Errorf("quota check failed (failing open): %w", err)
	}
	if v, ok := res.(int64); !ok || v == 0 {
		return ClassificationOverflow, noop, nil
	}

	release := func() {
		// Background context: the release must run even if the request
		// context is already canceled.
		_ = releaseScript.Run(context.Background(), q.rdb, []string{key}, q.cfg.WindowSeconds).Err()
	}
	return ClassificationReserved, release, nil
}
