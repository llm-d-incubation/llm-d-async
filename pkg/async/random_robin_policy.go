package async

import (
	"reflect"

	"github.com/llm-d-incubation/llm-d-async/pkg/async/api"
)

func NewRandomRobinPolicy() api.RequestMergePolicy {
	return &RandomRobinPolicy{}
}

type RandomRobinPolicy struct {
}

func (r *RandomRobinPolicy) MergeRequestChannels(channels []api.RequestChannel) api.EmbelishedRequestChannel {
	mergedChannel := make(chan api.EmbelishedRequestMessage)

	cases := make([]reflect.SelectCase, len(channels)) //nolint:staticcheck
	for i, ch := range channels {
		cases[i] = reflect.SelectCase{Dir: reflect.SelectRecv, Chan: reflect.ValueOf(ch.Channel)}
	}

	go func() {
		for {
			i1, val, ok := reflect.Select(cases)
			if !ok {
				// one of the channels is closed, remove it
				newCases := make([]reflect.SelectCase, 0, len(cases)-1)
				for i2, c := range cases {
					if i2 != i1 {
						newCases = append(newCases, c)
					}
				}
				cases = newCases
				if len(cases) == 0 {
					close(mergedChannel)
					break
				}
			} else {
				rm := val.Interface().(api.RequestMessage)
				mergedChannel <- api.EmbelishedRequestMessage{
					RequestMessage:     rm,
					OrgChannel:         channels[i1].Channel,
					InferenceObjective: channels[i1].Metadata["inference-objective"].(string),
					InferenceGateway:   channels[i1].Metadata["inference-gateway"].(string),
				}
			}

		}
	}()

	return api.EmbelishedRequestChannel{
		Channel: mergedChannel,
	}
}
