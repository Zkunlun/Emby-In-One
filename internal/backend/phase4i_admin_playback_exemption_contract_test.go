package backend

import (
	"net/http"
	"testing"
)

func TestPhase4IPlaybackLimiterBoundaryExemptsAdmin(t *testing.T) {
	upstream := phase1DPlaybackUpstream(t)
	defer upstream.Close()

	withTempAppConfig(t, phase1DPlaybackConfig(upstream.URL, 1), func(app *App, _ http.Handler) {
		serverID := app.Upstream.Clients()[0].ID

		adminCtx := &RequestContext{ProxyUser: &tokenInfo{UserID: "admin-id", Role: "admin"}}
		if userID, ok := app.playbackLimiterKey(adminCtx, serverID); ok || userID != "" {
			t.Fatalf("admin unexpectedly entered playback limiter boundary: userID=%q ok=%v", userID, ok)
		}

		regularCtx := &RequestContext{ProxyUser: &tokenInfo{UserID: "user-a", Role: "user"}}
		if userID, ok := app.playbackLimiterKey(regularCtx, serverID); !ok || userID != "user-a" {
			t.Fatalf("regular user did not enter playback limiter boundary: userID=%q ok=%v", userID, ok)
		}

		if got := app.PlaybackLimiter.CountForServer(serverID); got != 0 {
			t.Fatalf("boundary inspection must not create leases: got=%d", got)
		}
	})
}
