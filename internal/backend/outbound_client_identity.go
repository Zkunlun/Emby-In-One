package backend

import (
	"net/http"
	"net/url"
	"strings"
)

// outboundClientIdentity is a value snapshot of the configured upstream-facing
// identity. It never reads the real device identity from RequestContext.
// accept preserves the existing profile's default; it is not an identity field.
type outboundClientIdentity struct {
	UserAgent  string
	Client     string
	Version    string
	DeviceName string
	DeviceID   string
	accept     string
}

// configuredOutboundIdentity keeps the existing selection and empty-field
// semantics. Passthrough must continue through the real-identity resolver.
func (c *UpstreamClient) configuredOutboundIdentity() (outboundClientIdentity, bool) {
	cfg := c.Config
	if cfg.SpoofClient == "passthrough" {
		return outboundClientIdentity{}, false
	}
	if cfg.SpoofClient == "custom" {
		return outboundClientIdentity{
			UserAgent: cfg.CustomUserAgent, Client: cfg.CustomClient,
			Version: cfg.CustomClientVersion, DeviceName: cfg.CustomDeviceName,
			DeviceID: cfg.CustomDeviceId,
		}, true
	}
	profile := embyClientHeaders
	if preset, ok := spoofProfiles[cfg.SpoofClient]; ok {
		profile = preset
	}
	return outboundClientIdentity{
		UserAgent: profile["User-Agent"], Client: profile["X-Emby-Client"],
		Version: profile["X-Emby-Client-Version"], DeviceName: profile["X-Emby-Device-Name"],
		DeviceID: profile["X-Emby-Device-Id"], accept: profile["Accept"],
	}, true
}

func (identity outboundClientIdentity) headers() http.Header {
	headers := identity.applyHeaders(nil)
	if identity.accept != "" {
		headers.Set("Accept", identity.accept)
	}
	return headers
}

// applyHeaders runs after caller/extra-header merges, on an outbound copy.
// Clear every spelling/value of the identity headers, including compound
// headers, so an empty custom field cannot be filled from a real client value.
// The existing authentication preparer then rebuilds the compound header and
// upstream credentials; transport, content and language headers stay intact.
func (identity outboundClientIdentity) applyHeaders(headers http.Header) http.Header {
	prepared := cloneHeader(headers)
	for key := range prepared {
		for _, identityKey := range []string{
			"User-Agent", "X-Emby-Client", "X-Emby-Client-Version",
			"X-Emby-Device-Name", "X-Emby-Device-Id",
			"X-Emby-Authorization", "Authorization",
		} {
			if strings.EqualFold(key, identityKey) {
				delete(prepared, key)
				break
			}
		}
	}
	for _, field := range []struct{ name, value string }{
		{"User-Agent", identity.UserAgent}, {"X-Emby-Client", identity.Client},
		{"X-Emby-Client-Version", identity.Version}, {"X-Emby-Device-Name", identity.DeviceName},
		{"X-Emby-Device-Id", identity.DeviceID},
	} {
		if field.value != "" {
			prepared.Set(field.name, field.value)
		}
	}
	return prepared
}

// applyQuery normalizes only declared identity carriers on an outbound copy.
// Missing keys stay missing; empty configured fields remove recognized real
// values instead of borrowing identity or inventing defaults.
func (identity outboundClientIdentity) applyQuery(params url.Values, businessPath, method string, stream bool) (url.Values, bool) {
	prepared := copyValues(params)
	changed := false
	for _, field := range []struct{ name, value string }{
		{"X-Emby-Client", identity.Client}, {"X-Emby-Client-Version", identity.Version},
		{"X-Emby-Device-Name", identity.DeviceName}, {"X-Emby-Device-Id", identity.DeviceID},
	} {
		if normalizeClientQueryField(prepared, field.name, field.value) {
			changed = true
		}
	}
	if isCurrentDeviceQueryEndpoint(businessPath, method, stream) {
		if normalizeClientQueryField(prepared, "DeviceId", identity.DeviceID) {
			changed = true
		}
	}
	return prepared, changed
}

func normalizeClientQueryField(params url.Values, name, value string) bool {
	present, changed := false, false
	for key, values := range params {
		if !strings.EqualFold(key, name) {
			continue
		}
		present = true
		if key != name || value == "" || len(values) != 1 || values[0] != value {
			changed = true
		}
		delete(params, key)
	}
	if present && value != "" {
		params.Set(name, value)
	}
	return changed
}

// businessPath excludes the configured base's deployment prefix. Bare DeviceId
// is a request identity only on declared playback/cancel endpoints. In
// particular, stream=true alone is insufficient: image requests also use Stream.
func isCurrentDeviceQueryEndpoint(businessPath, method string, stream bool) bool {
	if businessPath == "/Videos/ActiveEncodings" {
		return method == http.MethodDelete
	}
	if businessPath == "/Videos/ActiveEncodings/Delete" {
		return method == http.MethodPost
	}
	if method != http.MethodGet && method != http.MethodHead {
		return false
	}
	parts := strings.Split(strings.TrimPrefix(businessPath, "/"), "/")
	if len(parts) < 3 || (parts[0] != "Videos" && parts[0] != "Audio") || parts[1] == "" {
		return false
	}
	for _, part := range parts[2:] {
		if part == "" || part == "." || part == ".." {
			return false
		}
	}
	// This is the existing GET /Videos/{itemId}/{rest...} or Audio proxy outlet,
	// including resources/segments already forwarded by EIO. It does not expand
	// the set of routes EIO owns or rewrite a redirect Location.
	if stream {
		return true
	}
	if len(parts) != 3 {
		return false
	}
	file := parts[2]
	switch file {
	case "stream", "master.m3u8":
		return true
	case "universal":
		return parts[0] == "Audio"
	case "live.m3u8", "main.m3u8":
		return method == http.MethodGet
	}
	if strings.HasPrefix(file, "universal.") {
		return parts[0] == "Audio" && len(file) > len("universal.")
	}
	// The official single StreamFileName route includes named media files.
	// Keep unknown extensionless operations outside this conservative matcher.
	dot := strings.LastIndexByte(file, '.')
	return dot > 0 && dot < len(file)-1
}
