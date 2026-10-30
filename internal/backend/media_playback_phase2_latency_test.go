package backend

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"sort"
	"testing"
	"time"
)

// Comparable synthetic network baseline: same endpoint, mapping and delays on
// the original and candidate branches. This is not a production/first-frame KPI.
func TestPhase2PlaybackInfoLatencyProfile(t *testing.T) {
	upstream := func(item, user string, wait time.Duration) *httptest.Server {
		return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			switch {
			case r.Method == http.MethodPost && r.URL.Path == "/Users/AuthenticateByName":
				_ = json.NewEncoder(w).Encode(map[string]any{"AccessToken": "token-" + item, "User": map[string]any{"Id": user}})
			case r.URL.Path == "/Items/"+item+"/PlaybackInfo":
				time.Sleep(wait)
				_ = json.NewEncoder(w).Encode(map[string]any{"PlaySessionId": "session-" + item, "MediaSources": []map[string]any{{"Id": "source-" + item, "Container": "mkv"}}})
			default:
				http.NotFound(w, r)
			}
		}))
	}
	a := upstream("movie-a", "user-a", 60*time.Millisecond)
	defer a.Close()
	b := upstream("movie-b", "user-b", 110*time.Millisecond)
	defer b.Close()
	withTempAppConfig(t, dualUpstreamConfig(a.URL, b.URL), func(app *App, handler http.Handler) {
		clients := app.Upstream.Clients()
		item := app.IDStore.GetOrCreateVirtualID("movie-a", clients[0].ID)
		app.IDStore.AssociateAdditionalInstance(item, "movie-b", clients[1].ID)
		token := loginToken(t, handler, "secret")
		durations := make([]time.Duration, 0, 30)
		for i := 0; i < 31; i++ {
			start := time.Now()
			rr := doJSONRequest(t, handler, http.MethodGet, "/Items/"+item+"/PlaybackInfo", nil, token)
			elapsed := time.Since(start)
			if rr.Code != http.StatusOK {
				t.Fatalf("sample %d status=%d: %s", i, rr.Code, rr.Body.String())
			}
			if i > 0 {
				durations = append(durations, elapsed)
			}
		}
		sort.Slice(durations, func(i, j int) bool { return durations[i] < durations[j] })
		t.Logf("NETWORK_LATENCY_PROFILE samples=%d p50_ms=%.1f p95_ms=%.1f min_ms=%.1f max_ms=%.1f", len(durations), float64(durations[14])/float64(time.Millisecond), float64(durations[28])/float64(time.Millisecond), float64(durations[0])/float64(time.Millisecond), float64(durations[29])/float64(time.Millisecond))
	})
}
