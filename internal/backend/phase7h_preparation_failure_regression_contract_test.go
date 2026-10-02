package backend

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
)

func phase7HPreparationUpstream(t *testing.T, sessionHits *atomic.Int32) *httptest.Server {
	t.Helper()
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodPost && r.URL.Path == "/Users/AuthenticateByName":
			_ = json.NewEncoder(w).Encode(map[string]any{
				"AccessToken": "tok-a",
				"User":        map[string]any{"Id": "user-a"},
			})
		case r.Method == http.MethodPost && (r.URL.Path == "/Sessions/Playing" || r.URL.Path == "/Sessions/Playing/Progress" || r.URL.Path == "/Sessions/Playing/Stopped"):
			sessionHits.Add(1)
			w.WriteHeader(http.StatusNoContent)
		default:
			http.NotFound(w, r)
		}
	}))
}

func phase7HClearUpstreamUserIdentity(client *UpstreamClient) {
	client.mu.Lock()
	client.UserID = ""
	client.onAuthError = nil
	client.mu.Unlock()
}

func phase7HCorruptUpstreamBaseURL(client *UpstreamClient) {
	client.mu.Lock()
	client.BaseURL = "http://%zz"
	client.mu.Unlock()
}

func phase7HAssertClientInputPreparationError(t *testing.T, rr *httptest.ResponseRecorder) {
	t.Helper()
	if rr.Code != http.StatusBadRequest {
		t.Fatalf("status=%d want=400 body=%s", rr.Code, rr.Body.String())
	}
	var payload map[string]any
	if err := json.Unmarshal(rr.Body.Bytes(), &payload); err != nil {
		t.Fatalf("decode preparation error: %v body=%s", err, rr.Body.String())
	}
	if payload["kind"] != "unparsable-url" || payload["field"] != "path" {
		t.Fatalf("preparation error body=%#v", payload)
	}
}

func phase7HAssertPreparationError(t *testing.T, rr *httptest.ResponseRecorder) {
	t.Helper()
	if rr.Code != http.StatusServiceUnavailable {
		t.Fatalf("status=%d want=503 body=%s", rr.Code, rr.Body.String())
	}
	var payload map[string]any
	if err := json.Unmarshal(rr.Body.Bytes(), &payload); err != nil {
		t.Fatalf("decode preparation error: %v body=%s", err, rr.Body.String())
	}
	if payload["kind"] != "missing-upstream-auth-state" || payload["field"] != "body.UserId" {
		t.Fatalf("preparation error body=%#v", payload)
	}
}

func TestPhase7HPlayingAndProgressPreparationFailureRemainFailClosed(t *testing.T) {
	for _, path := range []string{"/Sessions/Playing", "/Sessions/Playing/Progress"} {
		t.Run(path, func(t *testing.T) {
			var sessionHits atomic.Int32
			upstream := phase7HPreparationUpstream(t, &sessionHits)
			defer upstream.Close()

			withTempAppConfig(t, phase1DPlaybackConfig(upstream.URL, 1), func(app *App, handler http.Handler) {
				token, info, itemID, before := phase7BSeedSessionState(t, app, handler)
				phase7HClearUpstreamUserIdentity(app.Upstream.ClientByID("server-a"))

				rr := phase1ESessionPost(t, handler, path, token, "xbox-001", map[string]any{
					"ItemId":        itemID,
					"UserId":        info.UserID,
					"PositionTicks": int64(900),
					"RunTimeTicks":  int64(1000),
				})
				phase7HAssertPreparationError(t, rr)
				phase7BAssertNoLocalCommit(t, app, info, itemID, before)
				if got := sessionHits.Load(); got != 0 {
					t.Fatalf("preparation failure reached upstream session endpoint: hits=%d", got)
				}
			})
		})
	}
}

func TestPhase7HPlayingAndProgressClientInputPreparationFailureRemainFailClosed(t *testing.T) {
	for _, path := range []string{"/Sessions/Playing", "/Sessions/Playing/Progress"} {
		t.Run(path, func(t *testing.T) {
			var sessionHits atomic.Int32
			upstream := phase7HPreparationUpstream(t, &sessionHits)
			defer upstream.Close()

			withTempAppConfig(t, phase1DPlaybackConfig(upstream.URL, 1), func(app *App, handler http.Handler) {
				token, info, itemID, before := phase7BSeedSessionState(t, app, handler)
				phase7HCorruptUpstreamBaseURL(app.Upstream.ClientByID("server-a"))

				rr := phase7BPostSession(t, handler, path, token, itemID, 900)
				phase7HAssertClientInputPreparationError(t, rr)
				phase7BAssertNoLocalCommit(t, app, info, itemID, before)
				if got := sessionHits.Load(); got != 0 {
					t.Fatalf("client-input preparation failure reached upstream session endpoint: hits=%d", got)
				}
			})
		})
	}
}

func TestPhase7HStoppedPreparationFailureFinalizesOnlyExactLease(t *testing.T) {
	for _, tc := range []struct {
		name           string
		deviceID       string
		leaseSession   string
		stoppedSession string
		wantLeases     int
	}{
		{name: "matching owner", deviceID: "xbox-001", leaseSession: "play-a", stoppedSession: "play-a", wantLeases: 0},
		{name: "wrong device", deviceID: "phone-001", leaseSession: "play-a", stoppedSession: "play-a", wantLeases: 1},
		{name: "old play session", deviceID: "xbox-001", leaseSession: "play-new", stoppedSession: "play-old", wantLeases: 1},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var sessionHits atomic.Int32
			upstream := phase7HPreparationUpstream(t, &sessionHits)
			defer upstream.Close()

			withTempAppConfig(t, phase1DPlaybackConfig(upstream.URL, 1), func(app *App, handler http.Handler) {
				token, info, itemID, _ := phase7BSeedSessionState(t, app, handler)
				app.PlaybackLimiter.mu.Lock()
				entry := app.PlaybackLimiter.streams[streamKey{UserID: info.UserID, ServerID: "server-a"}]
				entry.PlaySessionID = tc.leaseSession
				app.PlaybackLimiter.mu.Unlock()
				owner := "token:" + token
				app.playbackRoutes.Activate(owner, itemID, "server-a", tc.leaseSession, tc.leaseSession)
				phase7HClearUpstreamUserIdentity(app.Upstream.ClientByID("server-a"))

				rr := phase1ESessionPost(t, handler, "/Sessions/Playing/Stopped", token, tc.deviceID, map[string]any{
					"ItemId":        itemID,
					"UserId":        info.UserID,
					"PlaySessionId": tc.stoppedSession,
					"PositionTicks": int64(600),
					"RunTimeTicks":  int64(1000),
				})
				phase7HAssertPreparationError(t, rr)
				progress := app.WatchStore.GetProgress(info.UserID, itemID)
				if progress == nil || progress.PositionTicks != 600 || progress.RuntimeTicks != 1000 {
					t.Fatalf("Stopped preparation failure did not finalize progress: %#v", progress)
				}
				if got := app.PlaybackLimiter.CountForServer("server-a"); got != tc.wantLeases {
					t.Fatalf("lease count=%d want=%d", got, tc.wantLeases)
				}
				if got := sessionHits.Load(); got != 0 {
					t.Fatalf("preparation failure reached upstream session endpoint: hits=%d", got)
				}
			})
		})
	}
}

func TestPhase7HStoppedClientInputPreparationFailureStillFinalizesLocalState(t *testing.T) {
	var sessionHits atomic.Int32
	upstream := phase7HPreparationUpstream(t, &sessionHits)
	defer upstream.Close()

	withTempAppConfig(t, phase1DPlaybackConfig(upstream.URL, 1), func(app *App, handler http.Handler) {
		token, info, itemID, _ := phase7BSeedSessionState(t, app, handler)
		phase7HCorruptUpstreamBaseURL(app.Upstream.ClientByID("server-a"))

		rr := phase1ESessionPost(t, handler, "/Sessions/Playing/Stopped", token, "xbox-001", map[string]any{
			"ItemId":        itemID,
			"PlaySessionId": "play-a",
			"PositionTicks": int64(650),
			"RunTimeTicks":  int64(1000),
		})
		phase7HAssertClientInputPreparationError(t, rr)
		progress := app.WatchStore.GetProgress(info.UserID, itemID)
		if progress == nil || progress.PositionTicks != 650 || progress.RuntimeTicks != 1000 {
			t.Fatalf("Stopped client-input preparation failure did not finalize progress: %#v", progress)
		}
		if got := app.PlaybackLimiter.CountForServer("server-a"); got != 0 {
			t.Fatalf("matching Stopped client-input preparation failure left lease count=%d want=0", got)
		}
		if got := sessionHits.Load(); got != 0 {
			t.Fatalf("client-input preparation failure reached upstream session endpoint: hits=%d", got)
		}
	})
}
