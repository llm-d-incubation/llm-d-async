package frontend

import (
	"context"
	"fmt"
	"log/slog"
	"strings"
	"sync"
	"time"

	"github.com/redis/go-redis/v9"
)

// resultWaiter implements the multiplexed wait-mode wake-up: one Redis
// pub/sub connection per frontend replica, on which each waiting handler
// subscribes to its own result key's keyspace-notification channel before
// parking. Same-replica delivery is structural: the only replica subscribed
// to a request's channel is the one holding its connection. Notifications
// are fire and forget, so waiters keep a slow backup poll.
type resultWaiter struct {
	rdb    *redis.Client
	pubsub *redis.PubSub
	prefix string // keyspace channel prefix, "__keyspace@<db>__:"

	mu      sync.Mutex
	waiters map[string]chan struct{} // channel name -> wake signal
}

// notificationsEnabled reports whether the Redis server's keyspace
// notifications cover list events on keyspace channels (flags K and l, or
// the A alias for all classes).
func notificationsEnabled(ctx context.Context, rdb *redis.Client) bool {
	res, err := rdb.ConfigGet(ctx, "notify-keyspace-events").Result()
	if err != nil {
		return false
	}
	flags := res["notify-keyspace-events"]
	return strings.Contains(flags, "K") && (strings.Contains(flags, "l") || strings.Contains(flags, "A"))
}

func newResultWaiter(ctx context.Context, rdb *redis.Client, db int, logger *slog.Logger) *resultWaiter {
	w := &resultWaiter{
		rdb:     rdb,
		prefix:  fmt.Sprintf("__keyspace@%d__:", db),
		waiters: make(map[string]chan struct{}),
	}
	w.pubsub = rdb.Subscribe(ctx)
	go w.receive(ctx, logger)
	return w
}

func (w *resultWaiter) receive(ctx context.Context, logger *slog.Logger) {
	ch := w.pubsub.Channel()
	for {
		select {
		case <-ctx.Done():
			_ = w.pubsub.Close()
			return
		case msg, ok := <-ch:
			if !ok {
				logger.Warn("result waiter pub/sub channel closed")
				return
			}
			w.mu.Lock()
			if wake, exists := w.waiters[msg.Channel]; exists {
				select {
				case wake <- struct{}{}:
				default: // waiter already signaled
				}
			}
			w.mu.Unlock()
		}
	}
}

// register subscribes to the key's notification channel and returns a wake
// channel plus a cleanup func. Callers must check the key AFTER register
// returns: a result that landed before the subscription is only found by
// that check.
func (w *resultWaiter) register(ctx context.Context, key string) (<-chan struct{}, func(), error) {
	channel := w.prefix + key
	wake := make(chan struct{}, 1)
	w.mu.Lock()
	w.waiters[channel] = wake
	w.mu.Unlock()
	if err := w.pubsub.Subscribe(ctx, channel); err != nil {
		w.mu.Lock()
		delete(w.waiters, channel)
		w.mu.Unlock()
		return nil, nil, err
	}
	cleanup := func() {
		w.mu.Lock()
		delete(w.waiters, channel)
		w.mu.Unlock()
		// Unsubscribe with a fresh context: cleanup runs on request exit,
		// when the request context may already be canceled.
		unsubCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = w.pubsub.Unsubscribe(unsubCtx, channel)
	}
	return wake, cleanup, nil
}
