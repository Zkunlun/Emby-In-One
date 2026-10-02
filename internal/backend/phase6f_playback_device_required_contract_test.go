package backend

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
)

func phase6FClearTokenDeviceIdentity(app *App, token string) {
	app.Auth.mu.Lock()
	info := app.Auth.tokens[token]
	info.DeviceID = ""
	app.Auth.tokens[token] = info
	app.Auth.mu.Unlock()
	app.Identity.DeleteCaptured(token)
}

func TestPhase6FMissingPlaybackDeviceIDFailsBeforeLeaseAndUpstream(t *testing.T) {
	var playbackInfoHits atomic.Int32
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodPost && r.URL.Path == "/Users/AuthenticateByName":
			_ = json.NewEncoder(w).Encode(map[string]any{"AccessToken": "tok-a", "User": map[string]any{"Id": "user-a"}})
		case r.URL.Path == "/Items/item-a/PlaybackInfo":
			playbackInfoHits.Add(1)
			_ = json.NewEncoder(w).Encode(map[string]any{
				"MediaSources":  []map[string]any{{"Id": "ms-a", "ItemId": "item-a", "Container": "mp4"}},
				"PlaySessionId": "play-a",
			})
		default:
			w.WriteHeader(http.StatusOK)
		}
	}))
	defer upstream.Close()

	withTempAppConfig(t, phase1DPlaybackConfig(upstream.URL, 1), func(app *App, handler http.Handler) {
		token := phase1ECreateAuthorizedUser(t, handler, []string{"server-a"})
		phase6FClearTokenDeviceIdentity(app, token)
		itemID := app.IDStore.GetOrCreateVirtualID("item-a", "server-a")

		req := httptest.NewRequest(http.MethodGet, "/Items/"+itemID+"/PlaybackInfo", nil)
		req.Header.Set("X-Emby-Token", token)
		rr := httptest.NewRecorder()
		handler.ServeHTTP(rr, req)

		if rr.Code != http.StatusBadRequest {
			t.Fatalf("missing DeviceID status=%d, want 400 body=%s", rr.Code, rr.Body.String())
		}
		if code := phase1GErrorCode(t, rr); code != playbackDeviceIDRequiredCode {
			t.Fatalf("missing DeviceID code=%q, want %s", code, playbackDeviceIDRequiredCode)
		}
		if got := playbackInfoHits.Load(); got != 0 {
			t.Fatalf("upstream PlaybackInfo hits=%d, want 0", got)
		}
		if got := app.PlaybackLimiter.CountForServer("server-a"); got != 0 {
			t.Fatalf("lease count=%d, want 0", got)
		}
	})
}

func TestPhase6FAdminPlaybackInfoWithoutDeviceIDRemainsExempt(t *testing.T) {
	var playbackInfoHits atomic.Int32
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodPost && r.URL.Path == "/Users/AuthenticateByName":
			_ = json.NewEncoder(w).Encode(map[string]any{"AccessToken": "tok-a", "User": map[string]any{"Id": "user-a"}})
		case r.URL.Path == "/Items/item-a/PlaybackInfo":
			playbackInfoHits.Add(1)
			_ = json.NewEncoder(w).Encode(map[string]any{
				"MediaSources":  []map[string]any{{"Id": "ms-a", "ItemId": "item-a", "Container": "mp4"}},
				"PlaySessionId": "play-a",
			})
		default:
			w.WriteHeader(http.StatusOK)
		}
	}))
	defer upstream.Close()

	withTempAppConfig(t, phase1DPlaybackConfig(upstream.URL, 1), func(app *App, handler http.Handler) {
		adminToken := loginTokenAs(t, handler, "admin", "secret")
		phase6FClearTokenDeviceIdentity(app, adminToken)
		itemID := app.IDStore.GetOrCreateVirtualID("item-a", "server-a")

		req := httptest.NewRequest(http.MethodGet, "/Items/"+itemID+"/PlaybackInfo", nil)
		req.Header.Set("X-Emby-Token", adminToken)
		rr := httptest.NewRecorder()
		handler.ServeHTTP(rr, req)

		if rr.Code != http.StatusOK {
			t.Fatalf("admin PlaybackInfo without DeviceID status=%d, want 200 body=%s", rr.Code, rr.Body.String())
		}
		if got := playbackInfoHits.Load(); got != 1 {
			t.Fatalf("admin upstream PlaybackInfo hits=%d, want 1", got)
		}
		if got := app.PlaybackLimiter.CountForServer("server-a"); got != 0 {
			t.Fatalf("admin lease count=%d, want 0", got)
		}
	})
}
