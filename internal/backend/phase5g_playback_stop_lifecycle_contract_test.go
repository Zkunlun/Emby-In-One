package backend

import (
	"net/http"
	"testing"
)

func TestPhase5GStoppedUsesResolvedDeviceAndTranslatedSession(t *testing.T) {
	upstream := phase1DPlaybackUpstream(t)
	defer upstream.Close()

	withTempAppConfig(t, phase1DPlaybackConfig(upstream.URL, 1), func(app *App, handler http.Handler) {
		token := phase1ECreateAuthorizedUser(t, handler, []string{"server-a"})
		info := app.Auth.ValidateToken(token)
		if info == nil {
			t.Fatal("regular token missing")
		}

		serverID := "server-a"
		virtualItemID := app.IDStore.GetOrCreateVirtualID("item-a", serverID)
		virtualSessionID := app.IDStore.GetOrCreateVirtualID("session-current", serverID)
		body := map[string]any{
			"ItemId":        virtualItemID,
			"PlaySessionId": virtualSessionID,
		}
		resolvedServerID, found := app.translateSessionBodyIDs(&RequestContext{ProxyUser: info, ProxyToken: token}, body)
		if !found || resolvedServerID != serverID {
			t.Fatalf("translated server = %q found=%v, want %q", resolvedServerID, found, serverID)
		}
		playSessionID, _ := body["PlaySessionId"].(string)
		if playSessionID != "session-current" {
			t.Fatalf("translated PlaySessionId = %q, want session-current", playSessionID)
		}

		created := app.PlaybackLimiter.Reserve(info.UserID, serverID, "xbox-001", virtualItemID, "session-current")
		if !created.Allowed || !created.Created {
			t.Fatalf("reserve = %+v, want created lease", created)
		}

		wrongDevice := &RequestContext{ProxyUser: info, PlaybackDeviceID: "phone-001"}
		if app.stopPlaybackLease(wrongDevice, serverID, playSessionID) {
			t.Fatal("wrong lifecycle device unexpectedly released lease")
		}
		if got := app.PlaybackLimiter.CountForServer(serverID); got != 1 {
			t.Fatalf("lease count after wrong-device stop = %d, want 1", got)
		}

		owner := &RequestContext{ProxyUser: info, PlaybackDeviceID: "xbox-001"}
		if app.stopPlaybackLease(owner, serverID, "session-old") {
			t.Fatal("stale PlaySessionID unexpectedly released current lease")
		}
		if got := app.PlaybackLimiter.CountForServer(serverID); got != 1 {
			t.Fatalf("lease count after stale-session stop = %d, want 1", got)
		}

		if !app.stopPlaybackLease(owner, serverID, playSessionID) {
			t.Fatal("owning device with current PlaySessionID should release lease")
		}
		if got := app.PlaybackLimiter.CountForServer(serverID); got != 0 {
			t.Fatalf("lease count after current stop = %d, want 0", got)
		}
	})
}
