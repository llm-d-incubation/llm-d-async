package fairness

import (
	"testing"

	"github.com/llm-d/llm-d-async/api"
)

func TestStamp(t *testing.T) {
	tests := []struct {
		name      string
		header    string
		attribute string
		metadata  map[string]string
		reqHeader map[string]string
		want      map[string]string
	}{
		{
			name:      "stamps the identity under the configured header",
			header:    api.FairnessIDHeader,
			attribute: "userid",
			metadata:  map[string]string{"userid": "tenant-a"},
			want:      map[string]string{api.FairnessIDHeader: "tenant-a"},
		},
		{
			name:      "reads the configured attribute, not the default",
			header:    "x-custom-fairness",
			attribute: "team",
			metadata:  map[string]string{"team": "team-b", "userid": "ignored"},
			want:      map[string]string{"x-custom-fairness": "team-b"},
		},
		{
			name:      "empty attribute falls back to the default",
			header:    api.FairnessIDHeader,
			attribute: "",
			metadata:  map[string]string{DefaultAttribute: "tenant-a"},
			want:      map[string]string{api.FairnessIDHeader: "tenant-a"},
		},
		{
			name:      "empty header disables stamping",
			header:    "",
			attribute: "userid",
			metadata:  map[string]string{"userid": "tenant-a"},
			want:      map[string]string{},
		},
		{
			name:      "attribute absent from metadata",
			header:    api.FairnessIDHeader,
			attribute: "userid",
			metadata:  map[string]string{"other": "x"},
			want:      map[string]string{},
		},
		{
			name:      "attribute present but empty",
			header:    api.FairnessIDHeader,
			attribute: "userid",
			metadata:  map[string]string{"userid": ""},
			want:      map[string]string{},
		},
		{
			name:      "caller-set header takes precedence",
			header:    api.FairnessIDHeader,
			attribute: "userid",
			metadata:  map[string]string{"userid": "tenant-a"},
			reqHeader: map[string]string{api.FairnessIDHeader: "caller-set"},
			want:      map[string]string{},
		},
		{
			// Stamping the canonical key alongside a caller's case variant would
			// leave two map keys collapsing onto one wire header.
			name:      "caller-set header takes precedence under any casing",
			header:    api.FairnessIDHeader,
			attribute: "userid",
			metadata:  map[string]string{"userid": "tenant-a"},
			reqHeader: map[string]string{"X-LLM-D-Inference-Fairness-ID": "caller-set"},
			want:      map[string]string{},
		},
		{
			// net/http rejects control characters at write time, which would fail
			// the whole request rather than just lose the fairness hint.
			name:      "identity that is not a legal header value is skipped",
			header:    api.FairnessIDHeader,
			attribute: "userid",
			metadata:  map[string]string{"userid": "tenant-a\r\nX-Injected: 1"},
			want:      map[string]string{},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			headers := map[string]string{}
			req := &api.RequestMessage{Metadata: tt.metadata, Headers: tt.reqHeader}

			New(tt.header, tt.attribute).Stamp(headers, req)

			if len(headers) != len(tt.want) {
				t.Fatalf("headers = %v, want %v", headers, tt.want)
			}
			for k, want := range tt.want {
				if got := headers[k]; got != want {
					t.Errorf("headers[%q] = %q, want %q", k, got, want)
				}
			}
		})
	}
}

func TestZeroStamperIsDisabled(t *testing.T) {
	headers := map[string]string{}
	req := &api.RequestMessage{Metadata: map[string]string{DefaultAttribute: "tenant-a"}}

	Stamper{}.Stamp(headers, req)

	if len(headers) != 0 {
		t.Errorf("zero Stamper wrote %v, want nothing", headers)
	}
}

func TestParamsHeaderOrDefault(t *testing.T) {
	empty := ""
	custom := "x-custom-fairness"

	tests := []struct {
		name   string
		params Params
		want   string
	}{
		{name: "absent parameter uses the default header", params: Params{}, want: api.FairnessIDHeader},
		{name: "explicit empty disables stamping", params: Params{Header: &empty}, want: ""},
		{name: "explicit header is used verbatim", params: Params{Header: &custom}, want: custom},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := tt.params.HeaderOrDefault(); got != tt.want {
				t.Errorf("HeaderOrDefault() = %q, want %q", got, tt.want)
			}
		})
	}
}
