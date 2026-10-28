package gateway

import (
	"net/http"
	"regexp"
	"strings"
)

// X-AGENT-SURFACE / X-AGENT-ID say where a call comes from and which app or agent made it, for usage reports.
// They are not auth (the API key is).
const (
	SurfaceHeader = "X-AGENT-SURFACE"
	AgentHeader   = "X-AGENT-ID"
)

var slugRE = regexp.MustCompile(`^[a-z0-9._-]{1,64}$`)

type Attribution struct {
	Surface string
	AgentID string
}

// normalizeSlug lowercases and validates a header value; ok is false for invalid values.
func normalizeSlug(raw string) (string, bool) {
	v := strings.ToLower(strings.TrimSpace(raw))
	return v, slugRE.MatchString(v)
}

// readAttribution parses both headers, or returns a 400 naming the missing or invalid ones.
func readAttribution(h http.Header) (Attribution, *APIError) {
	var missing, invalid []string
	values := map[string]string{}
	for _, name := range []string{SurfaceHeader, AgentHeader} {
		raw := strings.TrimSpace(h.Get(name))
		if raw == "" {
			missing = append(missing, name)
			continue
		}
		v, ok := normalizeSlug(raw)
		if !ok {
			invalid = append(invalid, name)
			continue
		}
		values[name] = v
	}
	if len(missing) > 0 {
		return Attribution{}, apiError(http.StatusBadRequest, "missing_header",
			"Missing required header "+strings.Join(missing, " and ")+". Send "+SurfaceHeader+
				" (where the call comes from, e.g. 'internal' or 'slack') and "+AgentHeader+
				" (a stable name for your app or agent).").with("headers_missing", missing)
	}
	if len(invalid) > 0 {
		return Attribution{}, apiError(http.StatusBadRequest, "invalid_header",
			"Invalid header "+strings.Join(invalid, " and ")+": use 1-64 characters of [a-z0-9._-].").
			with("headers_invalid", invalid)
	}
	return Attribution{Surface: values[SurfaceHeader], AgentID: values[AgentHeader]}, nil
}
