package backend

import (
	"net/http"
	"testing"
	"time"
)

func TestPhase5FHeartbeatUsesResolvedLifecycleDeviceIdentity(t *testing.T) {
	upstream := phase1DPlaybackUpstream(t)
	defer upstream.Close()

	withTempAppConfig(t, phase1DPlaybackConfig(upstream.URL, 1), func(app *App, handler http.Handler) {
		token := phase1ECreateAuthorizedUser(t, handler, []string{"server-a"})
		info := app.Auth.ValidateToken(token)
		if info == nil {
			t.Fatal("regular token missing")
		}

		serverID := "server-a"
		key := streamKey{UserID: info.UserID, ServerID: serverID}
		created := app.PlaybackLimiter.Reserve(info.UserID, serverID, "xbox-001", "item-a", "session-a")
		if !created.Allowed || !created.Created {
			t.Fatalf("reserve = %+v, want created lease", created)
		}

		old := time.Now().Add(-time.Minute).Round(0)
		app.PlaybackLimiter.mu.Lock()
		app.PlaybackLimiter.streams[key].LastHeartbeat = old
		app.PlaybackLimiter.mu.Unlock()

		ownerCtx := &RequestContext{ProxyUser: info, PlaybackDeviceID: "xbox-001"}
		if !app.heartbeatPlaybackLease(ownerCtx, serverID) {
			t.Fatal("owning lifecycle device should refresh heartbeat")
		}
		app.PlaybackLimiter.mu.Lock()
		ownerHeartbeat := app.PlaybackLimiter.streams[key].LastHeartbeat
		app.PlaybackLimiter.mu.Unlock()
		if !ownerHeartbeat.After(old) {
			t.Fatalf("owner heartbeat = %v, want after %v", ownerHeartbeat, old)
		}

		app.PlaybackLimiter.mu.Lock()
		app.PlaybackLimiter.streams[key].LastHeartbeat = old
		app.PlaybackLimiter.mu.Unlock()
		wrongCtx := &RequestContext{ProxyUser: info, PlaybackDeviceID: "phone-001"}
		if app.heartbeatPlaybackLease(wrongCtx, serverID) {
			t.Fatal("wrong lifecycle device unexpectedly refreshed heartbeat")
		}
		app.PlaybackLimiter.mu.Lock()
		wrongHeartbeat := app.PlaybackLimiter.streams[key].LastHeartbeat
		app.PlaybackLimiter.mu.Unlock()
		if !wrongHeartbeat.Equal(old) {
			t.Fatalf("wrong-device heartbeat changed timestamp: got=%v want=%v", wrongHeartbeat, old)
		}

		app.PlaybackLimiter.mu.Lock()
		app.PlaybackLimiter.streams[key].LastHeartbeat = time.Now().Add(-playbackHeartbeatTimeout - time.Second)
		app.PlaybackLimiter.mu.Unlock()
		if app.heartbeatPlaybackLease(ownerCtx, serverID) {
			t.Fatal("stale lease unexpectedly revived")
		}
		if got := app.PlaybackLimiter.CountForServer(serverID); got != 0 {
			t.Fatalf("stale lease remained after heartbeat: count=%d", got)
		}
	})
}
