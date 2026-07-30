package tierpriority

import (
	"encoding/json"
	"testing"
	"time"

	"github.com/llm-d/llm-d-async/api"
	"github.com/llm-d/llm-d-async/pipeline"
	"github.com/llm-d/llm-d-async/pkg/plugins"
)

// Stamping behavior itself is covered by the shared fairness package. These
// tests only pin that the policy wires the stamper into its dispatch path and
// resolves its parameters.

func mergeOne(t *testing.T, policy *TierPriorityPolicy) pipeline.EmbelishedRequestMessage {
	t.Helper()
	ch := pipeline.RequestChannel{
		Channel:      make(chan *api.InternalRequest, 1),
		WorkerPoolID: "pool-f",
		IGWBaseURL:   "http://gw",
	}
	pools := map[string]pipeline.WorkerPoolConfig{
		"pool-f": {ID: "pool-f", Workers: 1},
	}
	ch.Channel <- api.NewInternalRequest(api.InternalRouting{}, &api.RequestMessage{
		ID:       "m1",
		Created:  1,
		Deadline: 9999999999,
		Metadata: map[string]string{"userid": "tenant-a"},
	})
	close(ch.Channel)

	dispatch := policy.MergeRequestChannels([]pipeline.RequestChannel{ch}, pools)
	select {
	case msg := <-dispatch.Channels["pool-f"]:
		return msg
	case <-time.After(3 * time.Second):
		t.Fatal("timed out waiting for merged message")
		return pipeline.EmbelishedRequestMessage{}
	}
}

func TestFairnessHeaderStamped(t *testing.T) {
	policy := NewTierPriorityPolicy("test-policy", Config{
		PriorityHeader: "x-gateway-priority",
		TierLabel:      "tier",
		FairnessHeader: api.FairnessIDHeader,
	})

	msg := mergeOne(t, policy)
	if got := msg.HttpHeaders[api.FairnessIDHeader]; got != "tenant-a" {
		t.Errorf("fairness header = %q, want %q", got, "tenant-a")
	}
}

func TestFairnessHeaderDefaultsAndDisableViaConfig(t *testing.T) {
	factory, ok := plugins.Lookup("tier-priority")
	if !ok {
		t.Fatal("tier-priority plugin not registered")
	}

	// Absent parameters stamp the default header from the default attribute.
	plugin, err := factory("test", nil, nil)
	if err != nil {
		t.Fatalf("factory error: %v", err)
	}
	msg := mergeOne(t, plugin.(*TierPriorityPolicy))
	if got := msg.HttpHeaders[api.FairnessIDHeader]; got != "tenant-a" {
		t.Errorf("fairness header = %q, want %q", got, "tenant-a")
	}

	// An explicit empty header disables stamping.
	plugin, err = factory("test", json.RawMessage(`{"fairness_header": ""}`), nil)
	if err != nil {
		t.Fatalf("factory error: %v", err)
	}
	msg = mergeOne(t, plugin.(*TierPriorityPolicy))
	if _, stamped := msg.HttpHeaders[api.FairnessIDHeader]; stamped {
		t.Error("fairness header should be absent when disabled")
	}
}
