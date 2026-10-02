package backend

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

type phase7BRoundTripFunc func(*http.Request) (*http.Response, error)

func (f phase7BRoundTripFunc) RoundTrip(r *http.Request) (*http.Response, error) {
	return f(r)
}

func phase7BSessionUpstream(t *testing.T, sessionStatus int, sessionBody string) *httptest.Server {
	t.Helper()
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodPost && r.URL.Path == "/Users/AuthenticateByName":
			_ = json.NewEncoder(w).Encode(map[string]any{
				"AccessToken": "tok-a",
				"User":        map[string]any{"Id": "user-a"},
			})
		case r.Method == http.MethodPost && (r.URL.Path == "/Sessions/Playing" || r.URL.Path == "/Sessions/Playing/Progress" || r.URL.Path == "/Sessions/Playing/Stopped"):
			w.Header().Set("Content-Type", "text/plain")
			w.WriteHeader(sessionStatus)
			if sessionBody != "" {
				_, _ = w.Write([]byte(sessionBody))
			}
		default:
			http.NotFound(w, r)
		}
	}))
}

func phase7BSeedSessionState(t *testing.T, app *App, handler http.Handler) (string, *tokenInfo, string, time.Time) {
	t.Helper()
	token := phase1ECreateAuthorizedUser(t, handler, []string{"server-a"})
	info := app.Auth.ValidateToken(token)
	if info == nil {
		t.Fatal("regular token missing")
	}
	itemID := app.IDStore.GetOrCreateVirtualID("item-a", "server-a")
	if err := app.WatchStore.RecordProgress(&WatchProgress{
		ProxyUserID:    info.UserID,
		VirtualItemID:  itemID,
		ServerID:       "server-a",
		OriginalItemID: "item-a",
		ItemType:       "Movie",
		PositionTicks:  111,
		RuntimeTicks:   1000,
	}); err != nil {
		t.Fatalf("seed watch progress: %v", err)
	}
	reserved := app.PlaybackLimiter.Reserve(info.UserID, "server-a", "xbox-001", itemID, "play-a")
	if !reserved.Allowed || !reserved.Created {
		t.Fatalf("seed playback lease = %+v, want created lease", reserved)
	}
	before := time.Now().Add(-time.Minute)
	phase1EAgeLease(t, app, info.UserID, "server-a", before)
	return token, info, itemID, before
}

func phase7BPostSession(t *testing.T, handler http.Handler, path, token, itemID string, bodyPosition int64) *httptest.ResponseRecorder {
	t.Helper()
	return phase1ESessionPost(t, handler, path, token, "xbox-001", map[string]any{
		"ItemId":        itemID,
		"PositionTicks": bodyPosition,
		"RunTimeTicks":  int64(1000),
	})
}

func phase7BAssertNoLocalCommit(t *testing.T, app *App, info *tokenInfo, itemID string, before time.Time) {
	t.Helper()
	progress := app.WatchStore.GetProgress(info.UserID, itemID)
	if progress == nil || progress.PositionTicks != 111 || progress.RuntimeTicks != 1000 {
		t.Fatalf("local progress mutated after failed upstream confirmation: %#v", progress)
	}
	after := phase1ELeaseHeartbeat(t, app, info.UserID, "server-a")
	if !after.Equal(before) {
		t.Fatalf("failed upstream confirmation refreshed heartbeat: before=%v after=%v", before, after)
	}
}

func phase7BAssertSessionError(t *testing.T, rr *httptest.ResponseRecorder, wantStatus int, wantCode string) {
	t.Helper()
	if rr.Code != wantStatus {
		t.Fatalf("status=%d want=%d body=%s", rr.Code, wantStatus, rr.Body.String())
	}
	if code := phase1GErrorCode(t, rr); code != wantCode {
		t.Fatalf("code=%q want=%q body=%s", code, wantCode, rr.Body.String())
	}
}

func TestPhase7BPlayingAndProgressRequireUpstreamHTTPConfirmation(t *testing.T) {
	for _, path := range []string{"/Sessions/Playing", "/Sessions/Playing/Progress"} {
		t.Run(path, func(t *testing.T) {
			upstream := phase7BSessionUpstream(t, http.StatusInternalServerError, "sensitive-upstream-detail")
			defer upstream.Close()

			withTempAppConfig(t, phase1DPlaybackConfig(upstream.URL, 1), func(app *App, handler http.Handler) {
				token, info, itemID, before := phase7BSeedSessionState(t, app, handler)
				rr := phase7BPostSession(t, handler, path, token, itemID, 900)
				phase7BAssertSessionError(t, rr, http.StatusBadGateway, "UPSTREAM_SESSION_REJECTED")
				if strings.Contains(rr.Body.String(), "sensitive-upstream-detail") {
					t.Fatalf("upstream response body leaked to client: %s", rr.Body.String())
				}
				phase7BAssertNoLocalCommit(t, app, info, itemID, before)
			})
		})
	}
}

func TestPhase7BPlayingAndProgressFailClosedOnTransportFailure(t *testing.T) {
	for _, path := range []string{"/Sessions/Playing", "/Sessions/Playing/Progress"} {
		t.Run(path, func(t *testing.T) {
			upstream := phase7BSessionUpstream(t, http.StatusNoContent, "")
			defer upstream.Close()

			withTempAppConfig(t, phase1DPlaybackConfig(upstream.URL, 1), func(app *App, handler http.Handler) {
				token, info, itemID, before := phase7BSeedSessionState(t, app, handler)
				client := app.Upstream.ClientByID("server-a")
				client.httpClient.Transport = phase7BRoundTripFunc(func(*http.Request) (*http.Response, error) {
					return nil, errors.New("phase7b dial failure")
				})

				rr := phase7BPostSession(t, handler, path, token, itemID, 900)
				phase7BAssertSessionError(t, rr, http.StatusBadGateway, "UPSTREAM_SESSION_FAILED")
				phase7BAssertNoLocalCommit(t, app, info, itemID, before)
			})
		})
	}
}

func TestPhase7BPlayingAndProgressFailClosedOnUpstreamTimeout(t *testing.T) {
	for _, path := range []string{"/Sessions/Playing", "/Sessions/Playing/Progress"} {
		t.Run(path, func(t *testing.T) {
			upstream := phase7BSessionUpstream(t, http.StatusNoContent, "")
			defer upstream.Close()

			withTempAppConfig(t, phase1DPlaybackConfig(upstream.URL, 1), func(app *App, handler http.Handler) {
				token, info, itemID, before := phase7BSeedSessionState(t, app, handler)
				client := app.Upstream.ClientByID("server-a")
				client.httpClient.Transport = phase7BRoundTripFunc(func(*http.Request) (*http.Response, error) {
					return nil, context.DeadlineExceeded
				})

				rr := phase7BPostSession(t, handler, path, token, itemID, 900)
				phase7BAssertSessionError(t, rr, http.StatusGatewayTimeout, "UPSTREAM_SESSION_TIMEOUT")
				phase7BAssertNoLocalCommit(t, app, info, itemID, before)
			})
		})
	}
}

func TestPhase7BPlayingAndProgressDoNotCommitCanceledUpstreamRequest(t *testing.T) {
	for _, path := range []string{"/Sessions/Playing", "/Sessions/Playing/Progress"} {
		t.Run(path, func(t *testing.T) {
			upstream := phase7BSessionUpstream(t, http.StatusNoContent, "")
			defer upstream.Close()

			withTempAppConfig(t, phase1DPlaybackConfig(upstream.URL, 1), func(app *App, handler http.Handler) {
				token, info, itemID, before := phase7BSeedSessionState(t, app, handler)
				client := app.Upstream.ClientByID("server-a")
				client.httpClient.Transport = phase7BRoundTripFunc(func(*http.Request) (*http.Response, error) {
					return nil, context.Canceled
				})

				rr := phase7BPostSession(t, handler, path, token, itemID, 900)
				if rr.Code == http.StatusNoContent {
					t.Fatalf("canceled upstream request was reported as successful 204")
				}
				phase7BAssertNoLocalCommit(t, app, info, itemID, before)
			})
		})
	}
}

func TestPhase7BPlayingAndProgressFailClosedWhenUpstreamIsOffline(t *testing.T) {
	for _, path := range []string{"/Sessions/Playing", "/Sessions/Playing/Progress"} {
		t.Run(path, func(t *testing.T) {
			upstream := phase7BSessionUpstream(t, http.StatusNoContent, "")
			defer upstream.Close()

			withTempAppConfig(t, phase1DPlaybackConfig(upstream.URL, 1), func(app *App, handler http.Handler) {
				token, info, itemID, before := phase7BSeedSessionState(t, app, handler)
				app.Upstream.ClientByID("server-a").setOffline("phase7b offline")

				rr := phase7BPostSession(t, handler, path, token, itemID, 900)
				phase7BAssertSessionError(t, rr, http.StatusServiceUnavailable, "UPSTREAM_SESSION_UNAVAILABLE")
				phase7BAssertNoLocalCommit(t, app, info, itemID, before)
			})
		})
	}
}

func TestPhase7BNoContentForwardTreatsAny2xxAsConfirmed(t *testing.T) {
	upstream := phase7BSessionUpstream(t, http.StatusOK, "OK")
	defer upstream.Close()

	withTempAppConfig(t, phase1DPlaybackConfig(upstream.URL, 1), func(app *App, _ http.Handler) {
		req := httptest.NewRequest(http.MethodPost, "/Sessions/Playing/Progress", nil)
		client := app.Upstream.ClientByID("server-a")
		if err := app.forwardNoContent(req, client, http.MethodPost, "/Sessions/Playing/Progress", nil, map[string]any{"ItemId": "item-a"}); err != nil {
			t.Fatalf("2xx non-JSON response must count as upstream confirmation: %v", err)
		}
	})
}

func TestPhase7BStoppedOfflineStillFinalizesLocalStateAndExactLease(t *testing.T) {
	upstream := phase7BSessionUpstream(t, http.StatusNoContent, "")
	defer upstream.Close()

	withTempAppConfig(t, phase1DPlaybackConfig(upstream.URL, 1), func(app *App, handler http.Handler) {
		token, info, itemID, _ := phase7BSeedSessionState(t, app, handler)
		playSessionID := app.IDStore.GetOrCreateVirtualID("play-a", "server-a")
		app.Upstream.ClientByID("server-a").setOffline("phase7b offline")

		rr := phase1ESessionPost(t, handler, "/Sessions/Playing/Stopped", token, "xbox-001", map[string]any{
			"ItemId":        itemID,
			"PlaySessionId": playSessionID,
			"PositionTicks": int64(600),
			"RunTimeTicks":  int64(1000),
		})
		if rr.Code != http.StatusNoContent {
			t.Fatalf("offline Stopped status=%d want=204 body=%s", rr.Code, rr.Body.String())
		}
		progress := app.WatchStore.GetProgress(info.UserID, itemID)
		if progress == nil || progress.PositionTicks != 600 || progress.RuntimeTicks != 1000 {
			t.Fatalf("offline Stopped did not finalize local progress: %#v", progress)
		}
		if got := app.PlaybackLimiter.CountForServer("server-a"); got != 0 {
			t.Fatalf("offline Stopped left matching lease count=%d want=0", got)
		}
	})
}
