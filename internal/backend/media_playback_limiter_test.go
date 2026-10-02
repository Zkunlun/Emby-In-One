package backend

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strconv"
	"testing"
)

// playbackLimiterConfig gives the upstream one regular-user authorization slot.
// Playback-device leases are independent from maxConcurrent; these tests authenticate
// as the one authorized regular account.
func playbackLimiterConfig(url string) string {
	return `server:
  port: 8096
  name: "Test"
  id: "svr"
admin:
  username: "admin"
  password: "secret"
playback:
  mode: "proxy"
timeouts:
  api: 30000
  global: 15000
  login: 10000
  healthCheck: 10000
  healthInterval: 60000
proxies: []
upstream:
  - name: "A"
    url: ` + strconv.Quote(url) + `
    username: "u1"
    password: "p1"
    maxConcurrent: 1
`
}

// A PlaybackInfo request reserves a local playback lease before any upstream request.
// If every upstream attempt fails, that newly-created provisional lease must not be
// stranded for the three-minute heartbeat timeout.
func TestPlaybackInfoFailureReleasesPlaybackLease(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodPost && r.URL.Path == "/Users/AuthenticateByName":
			_ = json.NewEncoder(w).Encode(map[string]any{"AccessToken": "tok-a", "User": map[string]any{"Id": "user-a"}})
		default:
			// Every item request fails, so base stays nil and the handler answers 502.
			w.WriteHeader(http.StatusServiceUnavailable)
		}
	}))
	defer upstream.Close()

	withTempAppConfig(t, playbackLimiterConfig(upstream.URL), func(app *App, handler http.Handler) {
		userToken := createRegularUser(t, handler)
		serverID := app.Upstream.Clients()[0].ID
		virtualItem := app.IDStore.GetOrCreateVirtualID("item-a", serverID)

		rr := doAuthJSON(t, handler, http.MethodGet, "/Items/"+virtualItem+"/PlaybackInfo", nil, userToken)
		if rr.Code != http.StatusBadGateway {
			t.Fatalf("playback info status = %d, want 502 (body=%s)", rr.Code, rr.Body.String())
		}
		if got := app.PlaybackLimiter.CountForServer(serverID); got != 0 {
			t.Fatalf("playback lease leaked: CountForServer = %d, want 0 after a failed PlaybackInfo", got)
		}

		// A retry must behave identically rather than accumulating provisional leases.
		rr = doAuthJSON(t, handler, http.MethodGet, "/Items/"+virtualItem+"/PlaybackInfo", nil, userToken)
		if rr.Code != http.StatusBadGateway {
			t.Fatalf("retry status = %d, want 502", rr.Code)
		}
		if got := app.PlaybackLimiter.CountForServer(serverID); got != 0 {
			t.Fatalf("playback lease leaked on retry: CountForServer = %d, want 0", got)
		}
	})
}

// The success path must keep the playback lease until lifecycle events release it.
func TestPlaybackInfoSuccessKeepsPlaybackLease(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodPost && r.URL.Path == "/Users/AuthenticateByName":
			_ = json.NewEncoder(w).Encode(map[string]any{"AccessToken": "tok-a", "User": map[string]any{"Id": "user-a"}})
		case r.Method == http.MethodGet && r.URL.Path == "/Items/item-a/PlaybackInfo":
			_ = json.NewEncoder(w).Encode(map[string]any{
				"MediaSources":  []map[string]any{{"Id": "ms-1", "Container": "mp4"}},
				"PlaySessionId": "play-1",
			})
		default:
			http.NotFound(w, r)
		}
	}))
	defer upstream.Close()

	withTempAppConfig(t, playbackLimiterConfig(upstream.URL), func(app *App, handler http.Handler) {
		userToken := createRegularUser(t, handler)
		serverID := app.Upstream.Clients()[0].ID
		virtualItem := app.IDStore.GetOrCreateVirtualID("item-a", serverID)

		rr := doAuthJSON(t, handler, http.MethodGet, "/Items/"+virtualItem+"/PlaybackInfo", nil, userToken)
		if rr.Code != http.StatusOK {
			t.Fatalf("playback info status = %d, want 200 (body=%s)", rr.Code, rr.Body.String())
		}
		if got := app.PlaybackLimiter.CountForServer(serverID); got != 1 {
			t.Fatalf("CountForServer = %d, want 1 while a stream is active", got)
		}
	})
}
