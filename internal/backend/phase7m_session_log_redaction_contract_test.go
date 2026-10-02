package backend

import (
	"context"
	"errors"
	"net/http"
	"strings"
	"sync/atomic"
	"testing"
)

// Exercise the shared HTTP client's diagnostics as well as the lifecycle
// handlers. Handler-only redaction misses logs emitted before classification.
func TestPhase7MLifecycleDiagnosticsDoNotExposeUpstreamSecrets(t *testing.T) {
	paths := []string{"/Sessions/Playing", "/Sessions/Playing/Progress", "/Sessions/Playing/Stopped"}
	cases := []struct {
		name       string
		status     int
		transport  error
		wantStatus int
		wantCode   string
	}{
		{name: "success", status: http.StatusOK, wantStatus: http.StatusNoContent},
		{name: "rejected", status: http.StatusInternalServerError, wantStatus: http.StatusBadGateway, wantCode: upstreamSessionRejectedCode},
		{name: "transport-with-url", transport: errors.New("dial tcp https://phase7m-private.example:443?api_key=phase7m-private-token: refused"), wantStatus: http.StatusBadGateway, wantCode: upstreamSessionFailedCode},
		{name: "transport-without-url", transport: errors.New("dial tcp phase7m-private.example:443?api_key=phase7m-private-token: refused"), wantStatus: http.StatusBadGateway, wantCode: upstreamSessionFailedCode},
		{name: "net-timeout", transport: phase7ITimeoutError{}, wantStatus: http.StatusGatewayTimeout, wantCode: upstreamSessionTimeoutCode},
		{name: "deadline", transport: context.DeadlineExceeded, wantStatus: http.StatusGatewayTimeout, wantCode: upstreamSessionTimeoutCode},
		{name: "canceled", transport: context.Canceled, wantStatus: http.StatusBadGateway, wantCode: upstreamSessionFailedCode},
	}
	const responseSecret = "phase7m-private-response-body"

	for _, path := range paths {
		for _, tc := range cases {
			t.Run(path+"/"+tc.name, func(t *testing.T) {
				var sessionHits atomic.Int32
				var attempts atomic.Int32
				upstream := phase7ISessionUpstream(t, tc.status, responseSecret, &sessionHits)
				defer upstream.Close()

				withTempAppConfig(t, phase1DPlaybackConfig(upstream.URL, 1), func(app *App, handler http.Handler) {
					token, info, itemID, _ := phase7BSeedSessionState(t, app, handler)
					beforeProgress := app.WatchStore.GetProgress(info.UserID, itemID)
					beforeLease := phase7ISnapshotLease(t, app, info.UserID, "server-a")
					client := app.Upstream.ClientByID("server-a")
					if tc.transport != nil {
						client.httpClient.Transport = phase7BRoundTripFunc(func(*http.Request) (*http.Response, error) {
							attempts.Add(1)
							return nil, tc.transport
						})
					}

					// Authentication/setup have their own logging policy. Inspect all
					// levels emitted by this lifecycle operation, including the client.
					app.Logger.mu.Lock()
					app.Logger.buffer = nil
					app.Logger.mu.Unlock()

					rr := phase1ESessionPost(t, handler, path, token, "xbox-001", map[string]any{
						"ItemId": itemID, "PlaySessionId": "play-a",
						"PositionTicks": int64(400), "RunTimeTicks": int64(1000),
					})
					stopped := path == "/Sessions/Playing/Stopped"
					if stopped || tc.wantCode == "" {
						if rr.Code != http.StatusNoContent || rr.Body.Len() != 0 {
							t.Fatalf("status=%d body=%q, want empty 204", rr.Code, rr.Body.String())
						}
					} else {
						phase7BAssertSessionError(t, rr, tc.wantStatus, tc.wantCode)
					}

					if tc.transport != nil {
						if attempts.Load() != 1 || sessionHits.Load() != 0 {
							t.Fatalf("transport attempts=%d upstream hits=%d", attempts.Load(), sessionHits.Load())
						}
					} else if sessionHits.Load() != 1 {
						t.Fatalf("upstream hits=%d want=1", sessionHits.Load())
					}

					if !stopped && tc.wantCode != "" {
						phase7IAssertLocalStateUnchanged(t, app, info, itemID, beforeProgress, beforeLease)
					} else {
						progress := app.WatchStore.GetProgress(info.UserID, itemID)
						if progress == nil || progress.PositionTicks != 400 || progress.RuntimeTicks != 1000 {
							t.Fatalf("local progress=%#v, want 400/1000", progress)
						}
						if stopped {
							if count := app.PlaybackLimiter.CountForServer("server-a"); count != 0 {
								t.Fatalf("matching Stopped kept %d lease(s)", count)
							}
						} else {
							lease := phase7ISnapshotLease(t, app, info.UserID, "server-a")
							if lease.DeviceID != beforeLease.DeviceID || lease.PlaySessionID != beforeLease.PlaySessionID ||
								lease.Revision != beforeLease.Revision || lease.LastHeartbeat <= beforeLease.LastHeartbeat {
								t.Fatalf("successful lifecycle lease=%#v before=%#v", lease, beforeLease)
							}
						}
					}

					entries := app.Logger.Entries(0)
					if len(entries) == 0 {
						t.Fatal("no lifecycle diagnostics captured")
					}
					var messages []string
					for _, entry := range entries {
						messages = append(messages, entry.Message)
					}
					logText := strings.Join(messages, "\n")
					if !strings.Contains(logText, path) {
						t.Fatalf("fixed lifecycle route absent from diagnostics: %s", logText)
					}
					for _, secret := range []string{
						upstream.URL, token, "tok-a", responseSecret,
						"phase7m-private.example", "phase7m-private-token", "phase7i timeout secret",
						"http://", "https://",
					} {
						if strings.Contains(logText, secret) || strings.Contains(rr.Body.String(), secret) {
							t.Fatalf("lifecycle diagnostic/response contains forbidden detail %q", secret)
						}
					}
				})
			})
		}
	}
}
