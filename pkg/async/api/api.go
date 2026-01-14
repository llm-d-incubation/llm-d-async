package api

import "context"

type Flow interface {
	// starts processing requests.
	Start(ctx context.Context)

	// returns the channels for requests. Implementation is responsible for publishing on these channels.
	RequestChannels() []RequestChannel
	// returns the channel that accepts messages to be retries with their backoff delay. Implementation is responsible
	// for consuming messages on this channel.
	RetryChannel() chan RetryMessage
	// returns the channel for storing the results. Implementation is responsible for consuming messages on this channel.
	ResultChannel() chan ResultMessage
}

type RequestMergePolicy interface {
	MergeRequestChannels(channels []RequestChannel) EmbelishedRequestChannel
}

type RequestMessage struct {
	Id              string         `json:"id"`
	RetryCount      int            `json:"retry_count,omitempty"`
	DeadlineUnixSec string         `json:"deadline"`
	Payload         map[string]any `json:"payload"`
}

type RequestChannel struct {
	Channel chan RequestMessage
	// currently metadata is anything and the queue implementation should make use of it however it likes.
	Metadata map[string]any
}

type EmbelishedRequestChannel struct {
	Channel chan EmbelishedRequestMessage
}

type EmbelishedRequestMessage struct {
	RequestMessage
	OrgChannel chan RequestMessage
	// empty for none
	InferenceObjective string
	InferenceGateway   string
}

type RetryMessage struct {
	EmbelishedRequestMessage
	BackoffDurationSeconds float64
}

type ResultMessage struct {
	Id      string         `json:"id"`
	Payload map[string]any `json:"payload"`
}
