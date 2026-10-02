package backend

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"
)

func phase5DPlaybackFallbackUpstream(t *testing.T, token, userID, playSessionID string, failPlayback bool) *httptest.Server {
	t.Helper()
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodPost && r.URL.Path == "/Users/AuthenticateByName":
			_ = json.NewEncoder(w).Encode(map[string]any{
				"AccessToken": token,
				"User":        map[string]any{"Id": userID},
			})
		case (r.Method == http.MethodGet || r.Method == http.MethodPost) && r.URL.Path == "/Items/item-a/PlaybackInfo":
			if failPlayback {
				w.WriteHeader(http.StatusServiceUnavailable)
				return
			}
			_ = json.NewEncoder(w).Encode(map[string]any{
				"MediaSources":  []map[string]any{{"Id": "ms-b", "ItemId": "item-a", "Container": "mp4"}},
				"PlaySessionId": playSessionID,
			})
		default:
			http.NotFound(w, r)
		}
	}))
}

func TestPhase5DPlaybackInfoCommitsPrimaryServerSession(t *testing.T) {
	upstream := phase1ELifecycleUpstream(t, "tok-a", "user-a")
	defer upstream.Close()

	withTempAppConfig(t, phase1DPlaybackConfig(upstream.URL, 2), func(app *App, handler http.Handler) {
		token := phase1DCreateAuthorizedUser(t, handler)
		info := app.Auth.ValidateToken(token)
		if info == nil {
			t.Fatal("regular token missing")
		}
		serverID := app.Upstream.Clients()[0].ID
		item := app.IDStore.GetOrCreateVirtualID("item-a", serverID)

		rr := phase1DPlaybackInfo(t, handler, token, item, "xbox-001")
		if rr.Code != http.StatusOK {
			t.Fatalf("PlaybackInfo status=%d body=%s", rr.Code, rr.Body.String())
		}
		virtualSession := phase1EPlaySessionID(t, rr)
		resolvedSession := app.IDStore.ResolveVirtualID(virtualSession)
		if resolvedSession == nil || resolvedSession.OriginalID != "play-a" || resolvedSession.ServerID != serverID {
			t.Fatalf("PlaySessionId mapping = %+v, want play-a@%s", resolvedSession, serverID)
		}

		key := streamKey{UserID: info.UserID, ServerID: serverID}
		app.PlaybackLimiter.mu.Lock()
		entry := app.PlaybackLimiter.streams[key]
		app.PlaybackLimiter.mu.Unlock()
		if entry == nil || entry.ItemID != item || entry.PlaySessionID != "play-a" {
			t.Fatalf("committed primary lease = %+v, want item/play-a", entry)
		}
	})
}

func TestPhase5DPlaybackInfoCommitsFallbackServerSession(t *testing.T) {
	upstreamA := phase5DPlaybackFallbackUpstream(t, "tok-a", "user-a", "", true)
	defer upstreamA.Close()
	upstreamB := phase5DPlaybackFallbackUpstream(t, "tok-b", "user-b", "play-b", false)
	defer upstreamB.Close()

	config := fmt.Sprintf(`server:
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
    maxConcurrent: 2
  - id: "server-b"
    name: "B"
    url: %q
    username: "u2"
    password: "p2"
    maxConcurrent: 2
`, upstreamA.URL, upstreamB.URL)

	withTempAppConfig(t, config, func(app *App, handler http.Handler) {
		token := phase1ECreateAuthorizedUser(t, handler, []string{"server-a", "server-b"})
		info := app.Auth.ValidateToken(token)
		if info == nil {
			t.Fatal("regular token missing")
		}

		item := app.IDStore.GetOrCreateVirtualID("item-a", "server-a")
		app.IDStore.AssociateAdditionalInstance(item, "item-a", "server-b")

		rr := phase1DPlaybackInfo(t, handler, token, item, "xbox-001")
		if rr.Code != http.StatusOK {
			t.Fatalf("PlaybackInfo status=%d body=%s", rr.Code, rr.Body.String())
		}
		var payload map[string]any
		if err := json.Unmarshal(rr.Body.Bytes(), &payload); err != nil {
			t.Fatalf("decode PlaybackInfo: %v", err)
		}
		virtualSession, _ := payload["PlaySessionId"].(string)
		resolvedSession := app.IDStore.ResolveVirtualID(virtualSession)
		if resolvedSession == nil || resolvedSession.OriginalID != "play-b" || resolvedSession.ServerID != "server-b" {
			t.Fatalf("PlaySessionId mapping = %+v, want play-b@server-b", resolvedSession)
		}

		if got, ok := app.IDStore.GetActiveStream(item); !ok || got != "server-b" {
			t.Fatalf("active stream = %q ok=%v, want server-b", got, ok)
		}
		if got := app.PlaybackLimiter.CountForServer("server-a"); got != 0 {
			t.Fatalf("server-a provisional lease remained: %d", got)
		}
		if got := app.PlaybackLimiter.CountForServer("server-b"); got != 1 {
			t.Fatalf("server-b lease count = %d, want 1", got)
		}

		key := streamKey{UserID: info.UserID, ServerID: "server-b"}
		app.PlaybackLimiter.mu.Lock()
		entry := app.PlaybackLimiter.streams[key]
		app.PlaybackLimiter.mu.Unlock()
		if entry == nil || entry.ItemID != item || entry.PlaySessionID != "play-b" {
			t.Fatalf("committed fallback lease = %+v, want item/play-b on server-b", entry)
		}
	})
}
