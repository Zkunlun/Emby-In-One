package backend

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
)

func phase5HStoppedRequest(t *testing.T, app *App, info *tokenInfo, token, deviceID string) *http.Request {
	t.Helper()
	req := httptest.NewRequest(http.MethodPost, "/Sessions/Playing/Stopped", nil)
	reqCtx := &RequestContext{
		Headers:           req.Header.Clone(),
		ProxyToken:        token,
		ProxyUser:         info,
		PlaybackDeviceID:  deviceID,
		LegacyProxyUserID: app.Auth.ProxyUserID(),
		Identifiers:       app.newRequestIdentifierLookup(),
	}
	return req.WithContext(context.WithValue(req.Context(), requestContextKey{}, reqCtx))
}

func TestPhase5HStoppedPreparationErrorFinalizesMatchingLocalPlayback(t *testing.T) {
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
		if err := app.WatchStore.RecordProgress(&WatchProgress{
			ProxyUserID:    info.UserID,
			VirtualItemID:  virtualItemID,
			ServerID:       serverID,
			OriginalItemID: "item-a",
			ItemType:       "Movie",
		}); err != nil {
			t.Fatalf("seed watch progress: %v", err)
		}
		created := app.PlaybackLimiter.Reserve(info.UserID, serverID, "xbox-001", virtualItemID, "session-current")
		if !created.Allowed || !created.Created {
			t.Fatalf("reserve = %+v, want created lease", created)
		}

		body := map[string]any{
			"ItemId":        "item-a",
			"PlaySessionId": "session-current",
			"PositionTicks": float64(500),
			"RunTimeTicks":  float64(1000),
		}
		rr := httptest.NewRecorder()
		req := phase5HStoppedRequest(t, app, info, token, "xbox-001")
		prepErr := newMissingAuthStateError("body.UserId")
		if !app.handleStoppedPreparationError(rr, req, virtualItemID, body, serverID, "session-current", prepErr) {
			t.Fatal("preparation error was not handled")
		}
		if rr.Code != http.StatusServiceUnavailable {
			t.Fatalf("status = %d, want 503 body=%s", rr.Code, rr.Body.String())
		}
		var payload map[string]any
		if err := json.Unmarshal(rr.Body.Bytes(), &payload); err != nil {
			t.Fatalf("decode preparation error: %v", err)
		}
		if payload["kind"] != "missing-upstream-auth-state" || payload["field"] != "body.UserId" {
			t.Fatalf("preparation error body = %#v", payload)
		}
		progress := app.WatchStore.GetProgress(info.UserID, virtualItemID)
		if progress == nil || progress.PositionTicks != 500 || progress.RuntimeTicks != 1000 {
			t.Fatalf("local stopped progress = %#v, want position=500 runtime=1000", progress)
		}
		if got := app.PlaybackLimiter.CountForServer(serverID); got != 0 {
			t.Fatalf("matching stopped preparation error left lease count=%d, want 0", got)
		}
	})
}

func TestPhase5HStoppedPreparationErrorCannotReleaseNonMatchingLease(t *testing.T) {
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
		if err := app.WatchStore.RecordProgress(&WatchProgress{
			ProxyUserID:    info.UserID,
			VirtualItemID:  virtualItemID,
			ServerID:       serverID,
			OriginalItemID: "item-a",
			ItemType:       "Movie",
		}); err != nil {
			t.Fatalf("seed watch progress: %v", err)
		}
		created := app.PlaybackLimiter.Reserve(info.UserID, serverID, "xbox-001", virtualItemID, "session-current")
		if !created.Allowed || !created.Created {
			t.Fatalf("reserve = %+v, want created lease", created)
		}

		body := map[string]any{
			"ItemId":        "item-a",
			"PlaySessionId": "session-current",
			"PositionTicks": float64(600),
			"RunTimeTicks":  float64(1000),
		}
		rr := httptest.NewRecorder()
		req := phase5HStoppedRequest(t, app, info, token, "phone-001")
		if !app.handleStoppedPreparationError(rr, req, virtualItemID, body, serverID, "session-current", newMissingAuthStateError("body.UserId")) {
			t.Fatal("preparation error was not handled")
		}
		if rr.Code != http.StatusServiceUnavailable {
			t.Fatalf("status = %d, want 503", rr.Code)
		}
		progress := app.WatchStore.GetProgress(info.UserID, virtualItemID)
		if progress == nil || progress.PositionTicks != 600 {
			t.Fatalf("local progress was not recorded for wrong-device stop: %#v", progress)
		}
		if got := app.PlaybackLimiter.CountForServer(serverID); got != 1 {
			t.Fatalf("wrong-device preparation error released lease: count=%d want=1", got)
		}
	})
}
