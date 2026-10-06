package backend

import (
	"net/http"
	"strings"
	"sync/atomic"
	"testing"
)

// A second configured source is needed for partial hidden-library patches.
func withHomeLibraryPatchApp(t *testing.T, fn func(*App, http.Handler, *atomic.Int32)) {
	upstream, hits := homeLibraryUpstreamStub(t)
	cfg := strings.ReplaceAll(phase1EDualPlaybackConfig(upstream.URL, upstream.URL), "server-b", "other")
	withTempAppConfig(t, cfg, func(app *App, handler http.Handler) { fn(app, handler, hits) })
}

// Await the startup health cycle before cancelling its runner. Cancellation of
// a client request does not prove an accepted httptest handler has completed.
func phase5WaitInitialStreamProbes(t *testing.T, app *App) {
	t.Helper()
	client := app.Upstream.GetClient(0)
	if client == nil || len(client.StreamBaseURLs) < 2 {
		return
	}
	countsTestEventually(t, func() bool {
		client.mu.RLock()
		defer client.mu.RUnlock()
		for _, base := range client.StreamBaseURLs {
			if client.streamHealth[base].state == streamBaseUnknown {
				return false
			}
		}
		return true
	})
}

func seedPhase5RouteScope(app *App, reqCtx *RequestContext) {
	grants := []string{"server-a", "server-b"}
	app.ConfigStore = &ConfigStore{config: &Config{Upstream: []UpstreamConfig{{ID: "server-a"}, {ID: "server-b"}}}}
	reqCtx.ProxyUser.AllowedServers = grants
	app.UserStore = &UserStore{users: map[string]*User{reqCtx.ProxyUser.UserID: {
		ID: reqCtx.ProxyUser.UserID, Enabled: true, AllowedServers: grants, AuthRevision: reqCtx.ProxyUser.AuthRevision,
	}}}
}

// Seed a confirmed owner for tests focused on a later control event. A database
// history row and a limiter lease alone no longer prove a shared-state writer.
func seedPhase5WatchOwner(t *testing.T, app *App, userID, itemID, serverID, originalID, deviceID, sessionID, mediaID string, runtime int64) {
	t.Helper()
	source := playbackWatchSource{ServerID: serverID, OriginalItemID: originalID, MediaSourceID: mediaID}
	session := playbackWatchSession{ProxyUserID: userID, PlaybackDeviceID: deviceID, PlaySessionID: sessionID, Source: source}
	key := playbackWatchCacheKey{Session: session, VirtualItemID: itemID}
	metadata := WatchProgress{ProxyUserID: userID, VirtualItemID: itemID, ServerID: serverID, OriginalItemID: originalID, ItemType: "Movie"}
	app.watchPlayback.remember(key, metadata, playbackRuntimeCandidate{Source: source, Ticks: runtime, SingleMediaSource: true}, false)
	omitted := key
	omitted.Session.Source.MediaSourceID = ""
	app.watchPlayback.remember(omitted, metadata, playbackRuntimeCandidate{Source: source, Ticks: runtime, SingleMediaSource: true}, false)
	admission := app.watchPlayback.admit(key, playbackWatchStarted)
	if !app.watchPlayback.confirmStarted(admission, session) {
		t.Fatal("seed confirmed watch owner")
	}
}
