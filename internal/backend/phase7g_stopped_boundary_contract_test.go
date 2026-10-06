package backend

import (
	"context"
	"errors"
	"net/http"
	"testing"
)

func TestPhase7GStoppedUnavailableStillFinalizesMatchingLocalState(t *testing.T) {
	for _, availability := range []string{"offline", "missing"} {
		t.Run(availability, func(t *testing.T) {
			upstream := phase7BSessionUpstream(t, http.StatusNoContent, "")
			defer upstream.Close()

			withTempAppConfig(t, phase1DPlaybackConfig(upstream.URL, 1), func(app *App, handler http.Handler) {
				token, info, itemID, _ := phase7BSeedSessionState(t, app, handler)
				if availability == "offline" {
					app.Upstream.ClientByID("server-a").setOffline("phase7g offline")
				} else {
					phase7FRemoveUpstreamClient(app, "server-a")
				}

				rr := phase1ESessionPost(t, handler, "/Sessions/Playing/Stopped", token, "xbox-001", map[string]any{
					"ItemId":        itemID,
					"PlaySessionId": "play-a",
					"PositionTicks": int64(600),
					"RunTimeTicks":  int64(1000),
				})
				if rr.Code != http.StatusNoContent {
					t.Fatalf("Stopped status=%d want=204 body=%s", rr.Code, rr.Body.String())
				}
				progress := app.WatchStore.GetProgress(info.UserID, itemID)
				if progress == nil || progress.PositionTicks != 600 || progress.RuntimeTicks != 1000 {
					t.Fatalf("Stopped did not finalize local progress: %#v", progress)
				}
				if got := app.PlaybackLimiter.CountForServer("server-a"); got != 0 {
					t.Fatalf("matching unavailable Stopped left lease count=%d want=0", got)
				}
			})
		})
	}
}

func TestPhase7GStoppedUnavailableCannotReleaseWrongOwnerOrOldSession(t *testing.T) {
	for _, tc := range []struct {
		name           string
		deviceID       string
		leaseSession   string
		stoppedSession string
	}{
		{name: "wrong device", deviceID: "phone-001", leaseSession: "play-a", stoppedSession: "play-a"},
		{name: "old play session", deviceID: "xbox-001", leaseSession: "play-new", stoppedSession: "play-old"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			upstream := phase7BSessionUpstream(t, http.StatusNoContent, "")
			defer upstream.Close()

			withTempAppConfig(t, phase1DPlaybackConfig(upstream.URL, 1), func(app *App, handler http.Handler) {
				token, info, itemID, _ := phase7BSeedSessionState(t, app, handler)
				app.PlaybackLimiter.mu.Lock()
				entry := app.PlaybackLimiter.streams[streamKey{UserID: info.UserID, ServerID: "server-a"}]
				entry.PlaySessionID = tc.leaseSession
				app.PlaybackLimiter.mu.Unlock()
				owner := "token:" + token
				app.playbackRoutes.Activate(owner, itemID, "server-a", tc.leaseSession, tc.leaseSession)
				app.Upstream.ClientByID("server-a").setOffline("phase7g offline")

				rr := phase1ESessionPost(t, handler, "/Sessions/Playing/Stopped", token, tc.deviceID, map[string]any{
					"ItemId":        itemID,
					"PlaySessionId": tc.stoppedSession,
					"PositionTicks": int64(700),
					"RunTimeTicks":  int64(1000),
				})
				if rr.Code != http.StatusNoContent {
					t.Fatalf("Stopped status=%d want=204 body=%s", rr.Code, rr.Body.String())
				}
				progress := app.WatchStore.GetProgress(info.UserID, itemID)
				if progress == nil || progress.PositionTicks != 111 {
					t.Fatalf("non-matching Stopped changed current shared progress: %#v", progress)
				}
				if got := app.PlaybackLimiter.CountForServer("server-a"); got != 1 {
					t.Fatalf("non-matching Stopped released current lease: count=%d want=1", got)
				}
			})
		})
	}
}

func TestPhase7GStoppedGenericUpstreamFailureRemainsBestEffort(t *testing.T) {
	for _, tc := range []struct {
		name      string
		status    int
		transport error
	}{
		{name: "upstream rejected", status: http.StatusInternalServerError},
		{name: "transport failure", status: http.StatusNoContent, transport: errors.New("dial failure")},
		{name: "timeout", status: http.StatusNoContent, transport: context.DeadlineExceeded},
		{name: "canceled", status: http.StatusNoContent, transport: context.Canceled},
	} {
		t.Run(tc.name, func(t *testing.T) {
			upstream := phase7BSessionUpstream(t, tc.status, "upstream-sensitive-detail")
			defer upstream.Close()

			withTempAppConfig(t, phase1DPlaybackConfig(upstream.URL, 1), func(app *App, handler http.Handler) {
				token, info, itemID, _ := phase7BSeedSessionState(t, app, handler)
				if tc.transport != nil {
					client := app.Upstream.ClientByID("server-a")
					client.httpClient.Transport = phase7BRoundTripFunc(func(*http.Request) (*http.Response, error) {
						return nil, tc.transport
					})
				}

				rr := phase1ESessionPost(t, handler, "/Sessions/Playing/Stopped", token, "xbox-001", map[string]any{
					"ItemId":        itemID,
					"PlaySessionId": "play-a",
					"PositionTicks": int64(800),
					"RunTimeTicks":  int64(1000),
				})
				if rr.Code != http.StatusNoContent {
					t.Fatalf("Stopped status=%d want=204 body=%s", rr.Code, rr.Body.String())
				}
				progress := app.WatchStore.GetProgress(info.UserID, itemID)
				if progress == nil || progress.PositionTicks != 800 {
					t.Fatalf("generic failure Stopped did not finalize progress: %#v", progress)
				}
				if got := app.PlaybackLimiter.CountForServer("server-a"); got != 0 {
					t.Fatalf("generic failure Stopped left matching lease count=%d want=0", got)
				}
			})
		})
	}
}
