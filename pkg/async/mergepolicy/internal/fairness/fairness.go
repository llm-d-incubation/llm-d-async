// Package fairness stamps a tenant identity carried in request metadata into an
// HTTP header, so the gateway's flow control can arbitrate between tenants after
// dispatch. Both merge policies share it; stamping is best effort and never
// fails a request.
package fairness

import (
	"strings"

	"github.com/llm-d/llm-d-async/api"
)

// DefaultAttribute is the message metadata attribute stamped when a policy does
// not configure one. It matches the redis-quota gate's default so both sides key
// on the same tenant.
const DefaultAttribute = "userid"

// Params are the fairness parameters every merge policy accepts in its plugin
// configuration. Embed it in a policy's parameter struct; encoding/json promotes
// the embedded fields.
type Params struct {
	// Header is a pointer so an absent parameter (use the default header) is
	// distinguishable from an explicit "" (stamping disabled).
	Header    *string `json:"fairness_header"`
	Attribute string  `json:"fairness_attribute"`
}

// HeaderOrDefault returns the configured header name, falling back to
// api.FairnessIDHeader when the parameter was absent. An explicit "" is returned
// unchanged, since that disables stamping.
func (p Params) HeaderOrDefault() string {
	if p.Header == nil {
		return api.FairnessIDHeader
	}
	return *p.Header
}

// Stamper writes a tenant identity into an HTTP header. The zero Stamper is
// disabled and stamps nothing.
type Stamper struct {
	header    string
	attribute string
}

// New returns a Stamper that writes the tenant identity found under the given
// message metadata attribute into the given header. An empty header disables
// stamping; an empty attribute falls back to DefaultAttribute.
func New(header string, attribute string) Stamper {
	if attribute == "" {
		attribute = DefaultAttribute
	}
	return Stamper{header: header, attribute: attribute}
}

// Stamp writes the tenant identity from req's metadata into headers. It is a
// no-op when stamping is disabled, when req already carries the header, when the
// attribute is absent or empty, or when the identity is not a legal HTTP header
// value.
func (s Stamper) Stamp(headers map[string]string, req api.Request) {
	if s.header == "" {
		return
	}
	// HTTP header names are case-insensitive and net/http canonicalizes them on
	// write, so a caller-set header under any casing takes precedence: stamping
	// anyway would leave two map keys collapsing onto one wire header with
	// nondeterministic precedence.
	for k := range req.ReqHeaders() {
		if strings.EqualFold(k, s.header) {
			return
		}
	}
	id := req.ReqMetadata()[s.attribute]
	if id == "" || !validHeaderValue(id) {
		return
	}
	headers[s.header] = id
}

// validHeaderValue reports whether v can be written as an HTTP header value,
// mirroring what net/http enforces at write time. Metadata is caller-supplied,
// and net/http fails the entire request on a value it cannot write; a
// best-effort fairness hint must not cost the request, so an invalid identity
// skips the stamp instead.
func validHeaderValue(v string) bool {
	for i := range len(v) {
		if b := v[i]; (b < 0x20 && b != '\t') || b == 0x7f {
			return false
		}
	}
	return true
}
