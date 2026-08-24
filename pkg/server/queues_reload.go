package server

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"os"
	"time"

	"github.com/go-logr/logr"
	"github.com/llm-d/llm-d-async/pipeline"
	"github.com/llm-d/llm-d-async/pkg/metrics"
	"github.com/llm-d/llm-d-async/pkg/redis"
)

// startQueuesConfigReload periodically re-reads the sorted-set queues config
// file and applies queue set changes to the running flow: new and modified
// queues become consumable without a restart, removed queues are drained and
// their channels closed. The whole pipeline is fingerprint-gated — an
// unchanged file is a no-op, and a file that fails to read, parse or apply
// leaves the last good configuration exactly as it was.
//
// The merged channels observed by inference pool workers never change: the
// merge policy learns new sources through AddRequestChannels and forgets
// removed ones when the flow closes their channels.
func startQueuesConfigReload(
	ctx context.Context,
	path string,
	interval time.Duration,
	flow redis.SortedSetQueueReconfigurer,
	policy pipeline.DynamicRequestMergePolicy,
	pools map[string]pipeline.WorkerPoolConfig,
	logger logr.Logger,
) {
	logger = logger.WithName("queues-config-reload").WithValues("path", path, "interval", interval)
	go func() {
		ticker := time.NewTicker(interval)
		defer ticker.Stop()

		// Unknown initial fingerprint: the first successful pass applies the
		// file even when it matches the startup config — ReconfigureQueues is
		// idempotent for identical sets, so this only seeds lastApplied.
		var lastApplied [32]byte

		reload := func() {
			data, err := os.ReadFile(path) // #nosec G304 -- path from trusted CLI flag
			if err != nil {
				metrics.RecordQueueConfigReload(false)
				logger.Error(err, "Failed to read queues config file; keeping last good queues")
				return
			}
			fingerprint := sha256.Sum256(data)
			if fingerprint == lastApplied {
				return
			}

			var queues []redis.SortedSetQueueConfig
			if err := json.Unmarshal(data, &queues); err != nil {
				metrics.RecordQueueConfigReload(false)
				logger.Error(err, "Failed to parse queues config file; keeping last good queues")
				return
			}

			result, err := flow.ReconfigureQueues(queues)
			if err != nil {
				metrics.RecordQueueConfigReload(false)
				logger.Error(err, "Failed to apply queues config; keeping last good queues")
				return
			}
			if len(result.Added) > 0 {
				if err := policy.AddRequestChannels(result.Added, pools); err != nil {
					// The flow already consumes the new queues; without a
					// fan-in update their messages would sit in channels
					// nobody reads. Retrying the whole file on the next tick
					// is safe (unchanged queues stay untouched), so the
					// fingerprint deliberately stays uncommitted.
					metrics.RecordQueueConfigReload(false)
					logger.Error(err, "Failed to extend merge fan-in with new queues; retrying next tick", "added", len(result.Added))
					return
				}
			}

			lastApplied = fingerprint
			metrics.RecordQueueConfigReload(true)
			logger.Info("Applied queues config reload", "added", len(result.Added), "removed", len(result.Removed))
		}

		reload()
		for {
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
				reload()
			}
		}
	}()
}

// validateQueuesConfigWatch enforces that hot reload is only requested in a
// configuration where it can actually work: file-based queues config and a
// flow/policy pair that both support runtime changes. Called once at
// startup so a miswired deployment fails fast instead of drifting silently.
func validateQueuesConfigWatch(opts *Options, flow pipeline.Flow, policy pipeline.RequestMergePolicy) (redis.SortedSetQueueReconfigurer, pipeline.DynamicRequestMergePolicy, bool, error) {
	interval := opts.RedisSortedSet.QueuesConfigWatchInterval
	if interval <= 0 {
		return nil, nil, false, nil
	}
	if opts.RedisSortedSet.QueuesConfigFile == "" {
		return nil, nil, false, fmt.Errorf("--redis.ss.queues-config-watch-interval requires --redis.ss.queues-config-file")
	}
	reconfigurer, ok := flow.(redis.SortedSetQueueReconfigurer)
	if !ok {
		return nil, nil, false, fmt.Errorf("queues config hot reload requires the redis sorted-set transport")
	}
	dynamicPolicy, ok := policy.(pipeline.DynamicRequestMergePolicy)
	if !ok {
		return nil, nil, false, fmt.Errorf("queues config hot reload requires a merge policy supporting runtime channel changes")
	}
	return reconfigurer, dynamicPolicy, true, nil
}
