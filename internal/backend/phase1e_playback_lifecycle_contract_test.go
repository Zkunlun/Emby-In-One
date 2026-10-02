package backend

import (
	"bytes"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

func phase1ELifecycleUpstream(t *testing.T, token, userID string) *httptest.Server {
	t.Helper()
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodPost && r.URL.Path == "/Users/AuthenticateByName":
			_ = json.NewEncoder(w).Encode(map[string]any{
				"AccessToken": token,
				"User":        map[string]any{"Id": userID},
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
		case r.Method == http.MethodPost && (r.URL.Path == "/Sessions/Playing" || r.URL.Path == "/Sessions/Playing/Progress" || r.URL.Path == "/Sessions/Playing/Stopped"):
			w.WriteHeader(http.StatusNoContent)
		default:
			http.NotFound(w, r)
		}
	}))
}

func phase1EDualPlaybackConfig(urlA, urlB string) string {
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
    maxConcurrent: 1
  - id: "server-b"
    name: "B"
    url: %q
    username: "u2"
    password: "p2"
    maxConcurrent: 1
`, urlA, urlB)
}

func phase1ECreateAuthorizedUser(t *testing.T, handler http.Handler, allowedServers []string) string {
	t.Helper()
	adminToken := loginTokenAs(t, handler, "admin", "secret")
	rr := doAuthJSON(t, handler, http.MethodPost, "/admin/api/users", map[string]any{
		"username":       "alice",
		"password":       "password123",
		"allowedServers": allowedServers,
	}, adminToken)
	if rr.Code != http.StatusCreated {
		t.Fatalf("create user: status=%d body=%s", rr.Code, rr.Body.String())
	}
	return loginTokenAs(t, handler, "alice", "password123")
}

func phase1ESessionPost(t *testing.T, handler http.Handler, path, token, deviceID string, body map[string]any) *httptest.ResponseRecorder {
	t.Helper()
	encoded, err := json.Marshal(body)
	if err != nil {
		t.Fatalf("marshal session body: %v", err)
	}
	req := httptest.NewRequest(http.MethodPost, path, bytes.NewReader(encoded))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-Emby-Token", token)
	req.Header.Set("X-Emby-Client", "Phase1E")
	req.Header.Set("X-Emby-Client-Version", "1.0")
	req.Header.Set("X-Emby-Device-Name", deviceID)
	req.Header.Set("X-Emby-Device-Id", deviceID)
	rr := httptest.NewRecorder()
	handler.ServeHTTP(rr, req)
	return rr
}

func phase1EPlaySessionID(t *testing.T, rr *httptest.ResponseRecorder) string {
	t.Helper()
	var payload map[string]any
	if err := json.Unmarshal(rr.Body.Bytes(), &payload); err != nil {
		t.Fatalf("decode PlaybackInfo: %v body=%s", err, rr.Body.String())
	}
	playSessionID, _ := payload["PlaySessionId"].(string)
	if playSessionID == "" {
		t.Fatalf("PlaybackInfo missing PlaySessionId: %s", rr.Body.String())
	}
	return playSessionID
}

func phase1EAgeLease(t *testing.T, app *App, userID, serverID string, heartbeat time.Time) {
	t.Helper()
	app.PlaybackLimiter.mu.Lock()
	defer app.PlaybackLimiter.mu.Unlock()
	entry := app.PlaybackLimiter.streams[streamKey{UserID: userID, ServerID: serverID}]
	if entry == nil {
		t.Fatalf("playback lease missing for user=%s server=%s", userID, serverID)
	}
	entry.LastHeartbeat = heartbeat
}

func phase1ELeaseHeartbeat(t *testing.T, app *App, userID, serverID string) time.Time {
	t.Helper()
	app.PlaybackLimiter.mu.Lock()
	defer app.PlaybackLimiter.mu.Unlock()
	entry := app.PlaybackLimiter.streams[streamKey{UserID: userID, ServerID: serverID}]
	if entry == nil {
		t.Fatalf("playback lease missing for user=%s server=%s", userID, serverID)
	}
	return entry.LastHeartbeat
}

func TestPhase1ECurrentDeviceStoppedReleasesLease(t *testing.T) {
	upstream := phase1ELifecycleUpstream(t, "tok-a", "user-a")
	defer upstream.Close()

	withTempAppConfig(t, phase1DPlaybackConfig(upstream.URL, 1), func(app *App, handler http.Handler) {
		token := phase1ECreateAuthorizedUser(t, handler, []string{"server-a"})
		serverID := app.Upstream.Clients()[0].ID
		itemA := app.IDStore.GetOrCreateVirtualID("item-a", serverID)

		xbox := phase1DPlaybackInfo(t, handler, token, itemA, "xbox-001")
		if xbox.Code != http.StatusOK {
			t.Fatalf("Xbox PlaybackInfo: status=%d body=%s", xbox.Code, xbox.Body.String())
		}
		playSessionID := phase1EPlaySessionID(t, xbox)
		stopped := phase1ESessionPost(t, handler, "/Sessions/Playing/Stopped", token, "xbox-001", map[string]any{
			"ItemId":        itemA,
			"PlaySessionId": playSessionID,
		})
		if stopped.Code != http.StatusNoContent {
			t.Fatalf("Xbox Stopped: status=%d body=%s", stopped.Code, stopped.Body.String())
		}

		phone := phase1DPlaybackInfo(t, handler, token, itemA, "phone-001")
		if phone.Code != http.StatusOK {
			t.Fatalf("phone should take over immediately after Xbox stopped: status=%d body=%s", phone.Code, phone.Body.String())
		}
	})
}

func TestPhase1EWrongDeviceStoppedCannotReleaseLease(t *testing.T) {
	upstream := phase1ELifecycleUpstream(t, "tok-a", "user-a")
	defer upstream.Close()

	withTempAppConfig(t, phase1DPlaybackConfig(upstream.URL, 1), func(app *App, handler http.Handler) {
		token := phase1ECreateAuthorizedUser(t, handler, []string{"server-a"})
		serverID := app.Upstream.Clients()[0].ID
		itemA := app.IDStore.GetOrCreateVirtualID("item-a", serverID)

		xbox := phase1DPlaybackInfo(t, handler, token, itemA, "xbox-001")
		if xbox.Code != http.StatusOK {
			t.Fatalf("Xbox PlaybackInfo: status=%d body=%s", xbox.Code, xbox.Body.String())
		}
		playSessionID := phase1EPlaySessionID(t, xbox)
		_ = phase1ESessionPost(t, handler, "/Sessions/Playing/Stopped", token, "phone-001", map[string]any{
			"ItemId":        itemA,
			"PlaySessionId": playSessionID,
		})

		phone := phase1DPlaybackInfo(t, handler, token, itemA, "phone-001")
		if phone.Code == http.StatusOK {
			t.Fatalf("wrong-device Stopped released Xbox lease; phone PlaybackInfo unexpectedly succeeded: %s", phone.Body.String())
		}
	})
}

func TestPhase1EOldPlaySessionStoppedCannotReleaseNewerPlayback(t *testing.T) {
	upstream := phase1ELifecycleUpstream(t, "tok-a", "user-a")
	defer upstream.Close()

	withTempAppConfig(t, phase1DPlaybackConfig(upstream.URL, 1), func(app *App, handler http.Handler) {
		token := phase1ECreateAuthorizedUser(t, handler, []string{"server-a"})
		serverID := app.Upstream.Clients()[0].ID
		itemA := app.IDStore.GetOrCreateVirtualID("item-a", serverID)
		itemB := app.IDStore.GetOrCreateVirtualID("item-b", serverID)

		first := phase1DPlaybackInfo(t, handler, token, itemA, "xbox-001")
		if first.Code != http.StatusOK {
			t.Fatalf("first PlaybackInfo: status=%d body=%s", first.Code, first.Body.String())
		}
		oldPlaySessionID := phase1EPlaySessionID(t, first)

		newer := phase1DPlaybackInfo(t, handler, token, itemB, "xbox-001")
		if newer.Code != http.StatusOK {
			t.Fatalf("newer PlaybackInfo: status=%d body=%s", newer.Code, newer.Body.String())
		}
		newPlaySessionID := phase1EPlaySessionID(t, newer)
		if oldPlaySessionID == newPlaySessionID {
			t.Fatalf("test fixture did not create distinct PlaySessionIds: %q", oldPlaySessionID)
		}

		_ = phase1ESessionPost(t, handler, "/Sessions/Playing/Stopped", token, "xbox-001", map[string]any{
			"ItemId":        itemA,
			"PlaySessionId": oldPlaySessionID,
		})
		phone := phase1DPlaybackInfo(t, handler, token, itemB, "phone-001")
		if phone.Code == http.StatusOK {
			t.Fatalf("stale PlaySessionId released newer Xbox playback: %s", phone.Body.String())
		}
	})
}

func TestPhase1EWrongDeviceProgressCannotRefreshHeartbeat(t *testing.T) {
	upstream := phase1ELifecycleUpstream(t, "tok-a", "user-a")
	defer upstream.Close()

	withTempAppConfig(t, phase1DPlaybackConfig(upstream.URL, 1), func(app *App, handler http.Handler) {
		token := phase1ECreateAuthorizedUser(t, handler, []string{"server-a"})
		info := app.Auth.ValidateToken(token)
		if info == nil {
			t.Fatal("regular token missing")
		}
		serverID := app.Upstream.Clients()[0].ID
		itemA := app.IDStore.GetOrCreateVirtualID("item-a", serverID)

		xbox := phase1DPlaybackInfo(t, handler, token, itemA, "xbox-001")
		if xbox.Code != http.StatusOK {
			t.Fatalf("Xbox PlaybackInfo: status=%d body=%s", xbox.Code, xbox.Body.String())
		}
		playSessionID := phase1EPlaySessionID(t, xbox)
		oldHeartbeat := time.Now().Add(-time.Minute).Round(0)
		phase1EAgeLease(t, app, info.UserID, serverID, oldHeartbeat)

		progress := phase1ESessionPost(t, handler, "/Sessions/Playing/Progress", token, "phone-001", map[string]any{
			"ItemId":        itemA,
			"PlaySessionId": playSessionID,
			"PositionTicks": 1000,
		})
		if progress.Code != http.StatusNoContent {
			t.Fatalf("phone progress: status=%d body=%s", progress.Code, progress.Body.String())
		}
		if got := phase1ELeaseHeartbeat(t, app, info.UserID, serverID); !got.Equal(oldHeartbeat) {
			t.Fatalf("wrong device refreshed heartbeat: got=%v want=%v", got, oldHeartbeat)
		}
	})
}

func TestPhase1EExpiredLeaseAllowsAnotherDeviceToTakeOver(t *testing.T) {
	upstream := phase1ELifecycleUpstream(t, "tok-a", "user-a")
	defer upstream.Close()

	withTempAppConfig(t, phase1DPlaybackConfig(upstream.URL, 1), func(app *App, handler http.Handler) {
		token := phase1ECreateAuthorizedUser(t, handler, []string{"server-a"})
		info := app.Auth.ValidateToken(token)
		if info == nil {
			t.Fatal("regular token missing")
		}
		serverID := app.Upstream.Clients()[0].ID
		itemA := app.IDStore.GetOrCreateVirtualID("item-a", serverID)

		if rr := phase1DPlaybackInfo(t, handler, token, itemA, "xbox-001"); rr.Code != http.StatusOK {
			t.Fatalf("Xbox PlaybackInfo: status=%d body=%s", rr.Code, rr.Body.String())
		}
		phase1EAgeLease(t, app, info.UserID, serverID, time.Now().Add(-playbackHeartbeatTimeout-time.Second))

		phone := phase1DPlaybackInfo(t, handler, token, itemA, "phone-001")
		if phone.Code != http.StatusOK {
			t.Fatalf("expired lease should allow phone takeover: status=%d body=%s", phone.Code, phone.Body.String())
		}
	})
}

func TestPhase1EPlaybackDeviceLimitIsPerUpstream(t *testing.T) {
	upstreamA := phase1ELifecycleUpstream(t, "tok-a", "user-a")
	defer upstreamA.Close()
	upstreamB := phase1ELifecycleUpstream(t, "tok-b", "user-b")
	defer upstreamB.Close()

	withTempAppConfig(t, phase1EDualPlaybackConfig(upstreamA.URL, upstreamB.URL), func(app *App, handler http.Handler) {
		token := phase1ECreateAuthorizedUser(t, handler, []string{"server-a", "server-b"})
		itemA := app.IDStore.GetOrCreateVirtualID("item-a", "server-a")
		itemB := app.IDStore.GetOrCreateVirtualID("item-b", "server-b")

		if rr := phase1DPlaybackInfo(t, handler, token, itemA, "xbox-001"); rr.Code != http.StatusOK {
			t.Fatalf("Xbox on server A: status=%d body=%s", rr.Code, rr.Body.String())
		}
		if rr := phase1DPlaybackInfo(t, handler, token, itemB, "phone-001"); rr.Code != http.StatusOK {
			t.Fatalf("Phone on server B should be allowed concurrently: status=%d body=%s", rr.Code, rr.Body.String())
		}
	})
}

func TestPhase1EAdminRemainsPlaybackLimiterExempt(t *testing.T) {
	upstream := phase1ELifecycleUpstream(t, "tok-a", "user-a")
	defer upstream.Close()

	withTempAppConfig(t, phase1DPlaybackConfig(upstream.URL, 1), func(app *App, handler http.Handler) {
		adminToken := loginTokenAs(t, handler, "admin", "secret")
		serverID := app.Upstream.Clients()[0].ID
		itemA := app.IDStore.GetOrCreateVirtualID("item-a", serverID)

		if rr := phase1DPlaybackInfo(t, handler, adminToken, itemA, "xbox-001"); rr.Code != http.StatusOK {
			t.Fatalf("admin Xbox PlaybackInfo: status=%d body=%s", rr.Code, rr.Body.String())
		}
		if rr := phase1DPlaybackInfo(t, handler, adminToken, itemA, "phone-001"); rr.Code != http.StatusOK {
			t.Fatalf("admin Phone PlaybackInfo: status=%d body=%s", rr.Code, rr.Body.String())
		}
		if got := app.PlaybackLimiter.CountForServer(serverID); got != 0 {
			t.Fatalf("admin should not consume playback limiter entries: got=%d", got)
		}
	})
}
