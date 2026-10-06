package backend

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
)

func TestPhase6GMissingDeviceRejectsPlayingProgressAndStoppedWithoutMutatingLease(t *testing.T) {
	var playingHits atomic.Int32
	var progressHits atomic.Int32
	var stoppedHits atomic.Int32
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodPost && r.URL.Path == "/Users/AuthenticateByName":
			_ = json.NewEncoder(w).Encode(map[string]any{"AccessToken": "tok-a", "User": map[string]any{"Id": "user-a"}})
		case r.URL.Path == "/Sessions/Playing":
			playingHits.Add(1)
			w.WriteHeader(http.StatusNoContent)
		case r.URL.Path == "/Sessions/Playing/Progress":
			progressHits.Add(1)
			w.WriteHeader(http.StatusNoContent)
		case r.URL.Path == "/Sessions/Playing/Stopped":
			stoppedHits.Add(1)
			w.WriteHeader(http.StatusNoContent)
		default:
			w.WriteHeader(http.StatusOK)
		}
	}))
	defer upstream.Close()

	withTempAppConfig(t, phase1DPlaybackConfig(upstream.URL, 1), func(app *App, handler http.Handler) {
		token := phase1ECreateAuthorizedUser(t, handler, []string{"server-a"})
		info := app.Auth.ValidateToken(token)
		if info == nil {
			t.Fatal("issued token did not validate")
		}
		userID := info.UserID
		phase6FClearTokenDeviceIdentity(app, token)

		itemID := app.IDStore.GetOrCreateVirtualID("item-a", "server-a")
		playSessionID := app.IDStore.GetOrCreateVirtualID("play-a", "server-a")
		reserved := app.PlaybackLimiter.Reserve(userID, "server-a", "xbox-001", itemID, "play-a")
		if !reserved.Allowed {
			t.Fatal("seed playback lease was rejected")
		}
		before := phase1ELeaseHeartbeat(t, app, userID, "server-a")

		post := func(path string, body map[string]any) *httptest.ResponseRecorder {
			t.Helper()
			raw, err := json.Marshal(body)
			if err != nil {
				t.Fatalf("marshal body: %v", err)
			}
			req := httptest.NewRequest(http.MethodPost, path, bytes.NewReader(raw))
			req.Header.Set("Content-Type", "application/json")
			req.Header.Set("X-Emby-Token", token)
			rr := httptest.NewRecorder()
			handler.ServeHTTP(rr, req)
			return rr
		}

		baseBody := map[string]any{
			"ItemId":        itemID,
			"PlaySessionId": playSessionID,
			"PositionTicks": 1000,
			"RunTimeTicks":  10000,
		}

		playing := post("/Sessions/Playing", baseBody)
		if playing.Code != http.StatusBadRequest || phase1GErrorCode(t, playing) != playbackDeviceIDRequiredCode {
			t.Fatalf("Playing missing device: status=%d body=%s", playing.Code, playing.Body.String())
		}
		progress := post("/Sessions/Playing/Progress", baseBody)
		if progress.Code != http.StatusBadRequest || phase1GErrorCode(t, progress) != playbackDeviceIDRequiredCode {
			t.Fatalf("Progress missing device: status=%d body=%s", progress.Code, progress.Body.String())
		}
		if got := phase1ELeaseHeartbeat(t, app, userID, "server-a"); !got.Equal(before) {
			t.Fatalf("missing-device lifecycle refreshed heartbeat: before=%v after=%v", before, got)
		}

		stopped := post("/Sessions/Playing/Stopped", baseBody)
		if stopped.Code != http.StatusBadRequest || phase1GErrorCode(t, stopped) != playbackDeviceIDRequiredCode {
			t.Fatalf("Stopped missing device: status=%d body=%s", stopped.Code, stopped.Body.String())
		}
		if got := app.PlaybackLimiter.CountForServer("server-a"); got != 1 {
			t.Fatalf("missing-device Stopped released lease: count=%d, want 1", got)
		}
		if progress := app.WatchStore.GetProgress(userID, itemID); progress != nil {
			t.Fatalf("missing-device Stopped created unqualified watch progress: %#v", progress)
		}
		if got := playingHits.Load(); got != 0 {
			t.Fatalf("Playing upstream hits=%d, want 0", got)
		}
		if got := progressHits.Load(); got != 0 {
			t.Fatalf("Progress upstream hits=%d, want 0", got)
		}
		if got := stoppedHits.Load(); got != 0 {
			t.Fatalf("Stopped upstream hits=%d, want 0", got)
		}
	})
}

func TestPhase6GMissingDeviceCannotDirectlyHeartbeatOrStopLease(t *testing.T) {
	withTempApp(t, func(app *App, _ http.Handler) {
		ctx := &RequestContext{ProxyUser: &tokenInfo{UserID: "user-a", Role: "user"}}
		if result := app.PlaybackLimiter.Reserve("user-a", "server-a", "", "item-a", "play-a"); !result.Allowed {
			t.Fatal("seed empty-device lease was rejected")
		}
		if app.heartbeatPlaybackLease(ctx, "server-a") {
			t.Fatal("missing DeviceID unexpectedly heartbeated a lease")
		}
		if app.stopPlaybackLease(ctx, "server-a", "play-a") {
			t.Fatal("missing DeviceID unexpectedly stopped a lease")
		}
		if got := app.PlaybackLimiter.CountForServer("server-a"); got != 1 {
			t.Fatalf("missing DeviceID helper mutated lease count=%d, want 1", got)
		}
	})
}
