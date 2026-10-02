package backend

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"
)

func phase1DPlaybackConfig(url string, maxConcurrent int) string {
	return fmt.Sprintf(`server:
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
  - id: "server-a"
    name: "A"
    url: %q
    username: "u1"
    password: "p1"
    maxConcurrent: %d
`, url, maxConcurrent)
}

func phase1DPlaybackUpstream(t *testing.T) *httptest.Server {
	t.Helper()
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodPost && r.URL.Path == "/Users/AuthenticateByName":
			_ = json.NewEncoder(w).Encode(map[string]any{
				"AccessToken": "tok-a",
				"User":        map[string]any{"Id": "user-a"},
			})
		case (r.Method == http.MethodGet || r.Method == http.MethodPost) && r.URL.Path == "/Items/item-a/PlaybackInfo":
			_ = json.NewEncoder(w).Encode(map[string]any{
				"MediaSources":  []map[string]any{{"Id": "ms-a", "ItemId": "item-a", "Container": "mp4"}},
				"PlaySessionId": "play-a",
			})
		case (r.Method == http.MethodGet || r.Method == http.MethodPost) && r.URL.Path == "/Items/item-b/PlaybackInfo":
			_ = json.NewEncoder(w).Encode(map[string]any{
				"MediaSources":  []map[string]any{{"Id": "ms-b", "ItemId": "item-b", "Container": "mp4"}},
				"PlaySessionId": "play-b",
			})
		default:
			http.NotFound(w, r)
		}
	}))
}

func phase1DCreateAuthorizedUser(t *testing.T, handler http.Handler) string {
	t.Helper()
	adminToken := loginTokenAs(t, handler, "admin", "secret")
	rr := doAuthJSON(t, handler, http.MethodPost, "/admin/api/users", map[string]any{
		"username":       "alice",
		"password":       "password123",
		"allowedServers": []string{"server-a"},
	}, adminToken)
	if rr.Code != http.StatusCreated {
		t.Fatalf("create user: status=%d body=%s", rr.Code, rr.Body.String())
	}
	return loginTokenAs(t, handler, "alice", "password123")
}

func phase1DPlaybackInfo(t *testing.T, handler http.Handler, token, itemID, deviceID string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(http.MethodGet, "/Items/"+itemID+"/PlaybackInfo", nil)
	req.Header.Set("X-Emby-Token", token)
	req.Header.Set("X-Emby-Client", "Phase1D")
	req.Header.Set("X-Emby-Client-Version", "1.0")
	req.Header.Set("X-Emby-Device-Name", deviceID)
	req.Header.Set("X-Emby-Device-Id", deviceID)
	rr := httptest.NewRecorder()
	handler.ServeHTTP(rr, req)
	return rr
}

func TestPhase1DSinglePlaybackDeviceContract(t *testing.T) {
	upstream := phase1DPlaybackUpstream(t)
	defer upstream.Close()

	withTempAppConfig(t, phase1DPlaybackConfig(upstream.URL, 1), func(app *App, handler http.Handler) {
		token := phase1DCreateAuthorizedUser(t, handler)
		serverID := app.Upstream.Clients()[0].ID
		itemA := app.IDStore.GetOrCreateVirtualID("item-a", serverID)
		itemB := app.IDStore.GetOrCreateVirtualID("item-b", serverID)

		xboxFirst := phase1DPlaybackInfo(t, handler, token, itemA, "xbox-001")
		if xboxFirst.Code != http.StatusOK {
			t.Fatalf("Xbox first PlaybackInfo status=%d, want 200 body=%s", xboxFirst.Code, xboxFirst.Body.String())
		}
		if got := app.PlaybackLimiter.CountForServer(serverID); got != 1 {
			t.Fatalf("active playback count after Xbox start=%d, want 1", got)
		}

		// Same device may continue playback and move to another item / PlaySessionId.
		xboxNext := phase1DPlaybackInfo(t, handler, token, itemB, "xbox-001")
		if xboxNext.Code != http.StatusOK {
			t.Fatalf("Xbox next-item PlaybackInfo status=%d, want 200 body=%s", xboxNext.Code, xboxNext.Body.String())
		}
		if got := app.PlaybackLimiter.CountForServer(serverID); got != 1 {
			t.Fatalf("same device stacked playback count=%d, want 1", got)
		}

		// A second device for the same EIO user and same upstream must not play concurrently.
		phone := phase1DPlaybackInfo(t, handler, token, itemA, "phone-001")
		if phone.Code == http.StatusOK {
			t.Errorf("second device PlaybackInfo unexpectedly succeeded: status=%d body=%s", phone.Code, phone.Body.String())
		}

		// Rejecting the second device must not disturb the original device's ownership.
		xboxStillActive := phase1DPlaybackInfo(t, handler, token, itemA, "xbox-001")
		if xboxStillActive.Code != http.StatusOK {
			t.Fatalf("Xbox after rejected phone status=%d, want 200 body=%s", xboxStillActive.Code, xboxStillActive.Body.String())
		}
		if got := app.PlaybackLimiter.CountForServer(serverID); got != 1 {
			t.Fatalf("active playback count after rejected phone=%d, want 1", got)
		}
	})
}

func TestPhase1DDeviceLimitStillAppliesWhenMaxConcurrentIsUnlimited(t *testing.T) {
	upstream := phase1DPlaybackUpstream(t)
	defer upstream.Close()

	withTempAppConfig(t, phase1DPlaybackConfig(upstream.URL, 0), func(app *App, handler http.Handler) {
		token := phase1DCreateAuthorizedUser(t, handler)
		serverID := app.Upstream.Clients()[0].ID
		itemA := app.IDStore.GetOrCreateVirtualID("item-a", serverID)

		xbox := phase1DPlaybackInfo(t, handler, token, itemA, "xbox-001")
		if xbox.Code != http.StatusOK {
			t.Fatalf("Xbox PlaybackInfo status=%d, want 200 body=%s", xbox.Code, xbox.Body.String())
		}

		phone := phase1DPlaybackInfo(t, handler, token, itemA, "phone-001")
		if phone.Code == http.StatusOK {
			t.Fatalf("second device with maxConcurrent=0 unexpectedly succeeded: status=%d body=%s", phone.Code, phone.Body.String())
		}
	})
}
