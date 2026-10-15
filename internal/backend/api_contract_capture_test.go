package backend

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"sort"
	"strings"
	"sync"
	"testing"
)

// phase8BWireShape contains only request structure. It never stores user IDs,
// tokens, URLs, User-Agent values, credentials, or arbitrary request body values.
type phase8BWireShape struct {
	Method     string
	Path       string
	QueryKeys  []string
	HeaderKeys []string
	BodyFields map[string]string
}
type phase8BRecorder struct {
	mu      sync.Mutex
	records []phase8BWireShape
}

func (c *phase8BRecorder) snapshot() []phase8BWireShape {
	c.mu.Lock()
	defer c.mu.Unlock()
	out := make([]phase8BWireShape, len(c.records))
	copy(out, c.records)
	return out
}
func phase8BLabel(key string, kind string) string {
	safe := map[string]bool{
		"Id": true, "ItemId": true, "UserId": true, "MediaSourceId": true, "PlaySessionId": true,
		"SessionId": true, "SearchTerm": true, "ParentId": true, "Fields": true, "Recursive": true,
		"Limit": true, "StartIndex": true, "api_key": true, "EnableDirectPlay": true,
		"EnableDirectStream": true, "EnableTranscoding": true, "PositionTicks": true,
		"StartTimeTicks": true, "RunTimeTicks": true, "MaxStreamingBitrate": true,
		"PlayableMediaTypes": true, "SupportedCommands": true, "SupportsMediaControl": true,
		"SupportsSync": true, "DeviceProfile": true, "Username": true, "Pw": true,
		"Client": true, "Version": true, "Device": true, "DeviceId": true, "DeviceName": true,
		"Path": true, "MediaSources": true, "ProviderIds": true,
	}
	if safe[key] {
		return key
	}
	return "{" + kind + "}"
}
func phase8BRouteTemplate(raw string) string {
	// Template all request-controlled path segments, even if they look like
	// plausible IDs; only known Emby endpoint literals are retained.
	static := map[string]bool{
		"Items": true, "Users": true, "Videos": true, "Audio": true, "stream": true,
		"PlaybackInfo": true, "Sessions": true, "Capabilities": true,
		"Full": true, "Playing": true, "Progress": true, "Stopped": true,
		"Search": true, "Hints": true, "AuthenticateByName": true, "Views": true,
		"System": true, "Info": true, "Public": true,
	}
	parts := strings.Split(raw, "/")
	for i, p := range parts {
		if p != "" && !static[p] {
			parts[i] = "{segment}"
		}
	}
	return strings.Join(parts, "/")
}
func phase8BValueType(v any) string {
	switch v.(type) {
	case nil:
		return "null"
	case bool:
		return "bool"
	case string:
		return "string"
	case json.Number, float64:
		return "number"
	case []any:
		return "array"
	case map[string]any:
		return "object"
	default:
		return "other"
	}
}
func phase8BCaptureRequest(r *http.Request) phase8BWireShape {
	result := phase8BWireShape{Method: r.Method, Path: phase8BRouteTemplate(r.URL.Path), BodyFields: map[string]string{}}
	for k := range r.URL.Query() {
		result.QueryKeys = append(result.QueryKeys, phase8BLabel(k, "query-key"))
	}
	sort.Strings(result.QueryKeys)
	for k := range r.Header {
		ck := http.CanonicalHeaderKey(k)
		if ck == "Authorization" || ck == "X-Emby-Token" || ck == "X-Emby-Authorization" || ck == "User-Agent" || ck == "Content-Type" || ck == "Range" || ck == "Accept" || strings.HasPrefix(ck, "X-Emby-") {
			result.HeaderKeys = append(result.HeaderKeys, ck)
		} else {
			result.HeaderKeys = append(result.HeaderKeys, "{header}")
		}
	}
	sort.Strings(result.HeaderKeys)
	if r.Body != nil {
		var payload map[string]any
		dec := json.NewDecoder(io.LimitReader(r.Body, 1<<16))
		dec.UseNumber()
		if err := dec.Decode(&payload); err == nil {
			for k, v := range payload {
				result.BodyFields[phase8BLabel(k, "body-key")] = phase8BValueType(v)
			}
		} else if err != io.EOF {
			result.BodyFields["{body}"] = "opaque"
		}
	}
	return result
}
func phase8BUpstream(t *testing.T, status int, respond func(http.ResponseWriter, *http.Request)) (*httptest.Server, *phase8BRecorder) {
	t.Helper()
	capture := &phase8BRecorder{}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		record := phase8BCaptureRequest(r)
		capture.mu.Lock()
		capture.records = append(capture.records, record)
		capture.mu.Unlock()
		if respond != nil {
			respond(w, r)
			return
		}
		w.WriteHeader(status)
		if r.Method != http.MethodHead {
			_, _ = w.Write([]byte("{}"))
		}
	}))
	t.Cleanup(srv.Close)
	return srv, capture
}
func TestPhase8BRecorderRetainsOnlySafeWireShape(t *testing.T) {
	srv, capture := phase8BUpstream(t, http.StatusOK, nil)
	req, err := http.NewRequest(http.MethodPost, srv.URL+"/Items/PRIVATE-ITEM/PlaybackInfo?api_key=SECRETQUERY&UserId=PRIVATE-USER&really-secret=xyz",
		strings.NewReader(`{"UserId":"PRIVATE-USER","MediaSourceId":"PRIVATE-MEDIA","PositionTicks":9007199254740993,"secret-blob":"PRIVATE-BODY"}`))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Authorization", "Bearer SECRET-AUTH")
	req.Header.Set("User-Agent", "SECRET-REAL-CLIENT")
	req.Header.Set("X-Emby-Token", "SECRET-TOKEN")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	_ = resp.Body.Close()
	records := capture.snapshot()
	if len(records) != 1 {
		t.Fatalf("records=%d, want 1", len(records))
	}
	got := records[0]
	if got.Method != http.MethodPost || got.Path != "/Items/{segment}/PlaybackInfo" || got.BodyFields["PositionTicks"] != "number" {
		t.Fatalf("unexpected structural capture: %+v", got)
	}
	raw, _ := json.Marshal(records)
	for _, secret := range []string{"PRIVATE-ITEM", "PRIVATE-USER", "PRIVATE-MEDIA", "SECRETQUERY", "SECRET-AUTH", "SECRET-REAL-CLIENT", "SECRET-TOKEN", "PRIVATE-BODY", "really-secret"} {
		if strings.Contains(string(raw), secret) {
			t.Fatalf("recorder leaked sensitive data: %q", secret)
		}
	}
}
