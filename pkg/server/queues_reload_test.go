package server

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/go-logr/logr"
	"github.com/llm-d/llm-d-async/api"
	"github.com/llm-d/llm-d-async/pipeline"
	"github.com/llm-d/llm-d-async/pkg/redis"
)

type reconfigStub struct {
	mu     sync.Mutex
	calls  [][]redis.SortedSetQueueConfig
	result redis.QueueReconfigureResult
	err    error
}

func (s *reconfigStub) ReconfigureQueues(queues []redis.SortedSetQueueConfig) (redis.QueueReconfigureResult, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.calls = append(s.calls, queues)
	return s.result, s.err
}

func (s *reconfigStub) callCount() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return len(s.calls)
}

type dynPolicyStub struct {
	pipeline.RequestMergePolicy
	mu    sync.Mutex
	added [][]pipeline.RequestChannel
	err   error
}

func (s *dynPolicyStub) AddRequestChannels(channels []pipeline.RequestChannel, pools map[string]pipeline.WorkerPoolConfig) error {
	s.mu.Lock()
	s.added = append(s.added, channels)
	s.mu.Unlock()
	return s.err
}

func (s *dynPolicyStub) addCount() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return len(s.added)
}

func waitFor(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for %s", what)
		}
		time.Sleep(5 * time.Millisecond)
	}
}

func writeQueuesFile(t *testing.T, path, content string) {
	t.Helper()
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatalf("write queues file: %v", err)
	}
}

func TestQueuesConfigReload_AppliesChanges(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "queues.json")
	writeQueuesFile(t, path, `[{"id":"q1","queue_name":"q1","igw_base_url":"http://gw"}]`)

	flow := &reconfigStub{}
	policy := &dynPolicyStub{}
	pools := map[string]pipeline.WorkerPoolConfig{"default": {ID: "default", Workers: 1}}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	startQueuesConfigReload(ctx, path, 10*time.Millisecond, flow, policy, pools, logr.Discard())

	waitFor(t, "initial apply", func() bool { return flow.callCount() >= 1 })

	// A changed file is applied again.
	writeQueuesFile(t, path, `[{"id":"q1","queue_name":"q1","igw_base_url":"http://gw"},{"id":"q2","queue_name":"q2","igw_base_url":"http://gw"}]`)
	waitFor(t, "second apply", func() bool { return flow.callCount() >= 2 })

	if got := len(flow.calls[1]); got != 2 {
		t.Fatalf("second apply carried %d queues, want 2", got)
	}
}

func TestQueuesConfigReload_UnchangedFileIsNoop(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "queues.json")
	writeQueuesFile(t, path, `[{"id":"q1","queue_name":"q1","igw_base_url":"http://gw"}]`)

	flow := &reconfigStub{}
	policy := &dynPolicyStub{}
	pools := map[string]pipeline.WorkerPoolConfig{"default": {ID: "default", Workers: 1}}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	startQueuesConfigReload(ctx, path, 10*time.Millisecond, flow, policy, pools, logr.Discard())

	waitFor(t, "initial apply", func() bool { return flow.callCount() >= 1 })
	time.Sleep(100 * time.Millisecond)
	if got := flow.callCount(); got != 1 {
		t.Fatalf("unchanged file re-applied %d times, want 1", got)
	}
}

func TestQueuesConfigReload_BadContentKeepsLastGood(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "queues.json")
	writeQueuesFile(t, path, `[{"id":"q1","queue_name":"q1","igw_base_url":"http://gw"}]`)

	flow := &reconfigStub{}
	policy := &dynPolicyStub{}
	pools := map[string]pipeline.WorkerPoolConfig{"default": {ID: "default", Workers: 1}}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	startQueuesConfigReload(ctx, path, 10*time.Millisecond, flow, policy, pools, logr.Discard())

	waitFor(t, "initial apply", func() bool { return flow.callCount() >= 1 })

	writeQueuesFile(t, path, `{not json`)
	time.Sleep(150 * time.Millisecond)
	if got := flow.callCount(); got != 1 {
		t.Fatalf("broken file applied, callCount=%d", got)
	}

	// Recovery: valid new content applies again.
	writeQueuesFile(t, path, `[{"id":"q3","queue_name":"q3","igw_base_url":"http://gw"}]`)
	waitFor(t, "recovery apply", func() bool { return flow.callCount() >= 2 })
	if got := flow.calls[1][0].ID; got != "q3" {
		t.Fatalf("recovery carried %q, want q3", got)
	}
}

func TestQueuesConfigReload_ReconfigureErrorKeepsFingerprint(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "queues.json")
	writeQueuesFile(t, path, `[{"id":"q1","queue_name":"q1","igw_base_url":"http://gw"}]`)

	flow := &reconfigStub{err: errors.New("boom")}
	policy := &dynPolicyStub{}
	pools := map[string]pipeline.WorkerPoolConfig{"default": {ID: "default", Workers: 1}}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	startQueuesConfigReload(ctx, path, 10*time.Millisecond, flow, policy, pools, logr.Discard())

	waitFor(t, "first failed apply", func() bool { return flow.callCount() >= 1 })
	waitFor(t, "retries because fingerprint uncommitted", func() bool { return flow.callCount() >= 3 })
	if got := policy.addCount(); got != 0 {
		t.Fatalf("policy must not be extended on a failed apply, got %d calls", got)
	}
}

func TestQueuesConfigReload_FanInFailureRetriesWholeFile(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "queues.json")
	writeQueuesFile(t, path, `[{"id":"q1","queue_name":"q1","igw_base_url":"http://gw"}]`)

	flow := &reconfigStub{result: redis.QueueReconfigureResult{
		Added: []pipeline.RequestChannel{{Channel: make(chan *api.InternalRequest)}},
	}}
	policy := &dynPolicyStub{err: errors.New("fan-in broken")}
	pools := map[string]pipeline.WorkerPoolConfig{"default": {ID: "default", Workers: 1}}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	startQueuesConfigReload(ctx, path, 10*time.Millisecond, flow, policy, pools, logr.Discard())

	// Policy failure leaves the fingerprint uncommitted, so the same file is
	// re-submitted on the next tick until the policy accepts it.
	waitFor(t, "retry loop", func() bool { return flow.callCount() >= 2 })
}
