package backend

import (
	"context"
	"encoding/json"
	"errors"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
)

type phase7ITimeoutError struct{}

func (phase7ITimeoutError) Error() string   { return "phase7i timeout secret" }
func (phase7ITimeoutError) Timeout() bool   { return true }
func (phase7ITimeoutError) Temporary() bool { return true }

var _ net.Error = phase7ITimeoutError{}

type phase7ILeaseSnapshot struct {
	DeviceID      string
	ItemID        string
	PlaySessionID string
	Revision      uint64
	LastHeartbeat int64
}

func phase7ISnapshotLease(t *testing.T, app *App, userID, serverID string) phase7ILeaseSnapshot {
	t.Helper()
	app.PlaybackLimiter.mu.Lock()
	defer app.PlaybackLimiter.mu.Unlock()
	entry, ok := app.PlaybackLimiter.streams[streamKey{UserID: userID, ServerID: serverID}]
	if !ok {
		t.Fatalf("missing playback lease for user=%s server=%s", userID, serverID)
	}
	return phase7ILeaseSnapshot{
		DeviceID:      entry.DeviceID,
		ItemID:        entry.ItemID,
		PlaySessionID: entry.PlaySessionID,
		Revision:      entry.Revision,
		LastHeartbeat: entry.LastHeartbeat.UnixNano(),
	}
}

func phase7IAssertLocalStateUnchanged(t *testing.T, app *App, info *tokenInfo, itemID string, beforeProgress *WatchProgress, beforeLease phase7ILeaseSnapshot) {
	t.Helper()
	progress := app.WatchStore.GetProgress(info.UserID, itemID)
	if progress == nil || beforeProgress == nil {
		t.Fatalf("progress missing: before=%#v after=%#v", beforeProgress, progress)
	}
	if progress.PositionTicks != beforeProgress.PositionTicks || progress.RuntimeTicks != beforeProgress.RuntimeTicks || progress.LastPlayed != beforeProgress.LastPlayed || progress.UpdatedAt != beforeProgress.UpdatedAt {
		t.Fatalf("watch state mutated after failed upstream confirmation: before=%#v after=%#v", beforeProgress, progress)
	}
	afterLease := phase7ISnapshotLease(t, app, info.UserID, "server-a")
	if count := app.PlaybackLimiter.CountForServer("server-a"); count != 1 {
		t.Fatalf("failed upstream confirmation changed lease count=%d want=1", count)
	}
	if afterLease != beforeLease {
		t.Fatalf("playback lease mutated after failed upstream confirmation: before=%+v after=%+v", beforeLease, afterLease)
	}
}

func phase7ISessionUpstream(t *testing.T, sessionStatus int, sessionBody string, sessionHits *atomic.Int32) *httptest.Server {
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

func phase7IAssertSecretAbsentFromLogs(t *testing.T, app *App, secret string) {
	t.Helper()
	if secret == "" || app.Logger == nil {
		return
	}
	for _, entry := range app.Logger.Entries(0) {
		if strings.Contains(entry.Message, secret) {
			t.Fatalf("sensitive upstream detail leaked to log: %s", entry.Message)
		}
	}
}

func TestPhase7IPlayingAndProgressTransportHTTPFailureMatrix(t *testing.T) {
	paths := []string{"/Sessions/Playing", "/Sessions/Playing/Progress"}
	cases := []struct {
		name       string
		status     int
		body       string
		transport  error
		wantStatus int
		wantCode   string
		secret     string
	}{
		{name: "transport", status: http.StatusNoContent, transport: errors.New("dial tcp phase7i-secret.example:443?api_key=phase7i-secret-token: refused"), wantStatus: http.StatusBadGateway, wantCode: upstreamSessionFailedCode, secret: "phase7i-secret-token"},
		{name: "transport-host", status: http.StatusNoContent, transport: errors.New("dial tcp phase7i-secret-host.example:443: refused"), wantStatus: http.StatusBadGateway, wantCode: upstreamSessionFailedCode, secret: "phase7i-secret-host.example"},
		{name: "canceled", status: http.StatusNoContent, transport: context.Canceled, wantStatus: http.StatusBadGateway, wantCode: upstreamSessionFailedCode},
		{name: "deadline", status: http.StatusNoContent, transport: context.DeadlineExceeded, wantStatus: http.StatusGatewayTimeout, wantCode: upstreamSessionTimeoutCode},
		{name: "net-timeout", status: http.StatusNoContent, transport: phase7ITimeoutError{}, wantStatus: http.StatusGatewayTimeout, wantCode: upstreamSessionTimeoutCode, secret: "phase7i timeout secret"},
		{name: "http-401", status: http.StatusUnauthorized, body: "phase7i-upstream-secret-401", wantStatus: http.StatusBadGateway, wantCode: upstreamSessionRejectedCode, secret: "phase7i-upstream-secret-401"},
		{name: "http-403", status: http.StatusForbidden, body: "phase7i-upstream-secret-403", wantStatus: http.StatusBadGateway, wantCode: upstreamSessionRejectedCode, secret: "phase7i-upstream-secret-403"},
		{name: "http-500", status: http.StatusInternalServerError, body: "phase7i-upstream-secret-500", wantStatus: http.StatusBadGateway, wantCode: upstreamSessionRejectedCode, secret: "phase7i-upstream-secret-500"},
		{name: "http-502", status: http.StatusBadGateway, body: "phase7i-upstream-secret-502", wantStatus: http.StatusBadGateway, wantCode: upstreamSessionRejectedCode, secret: "phase7i-upstream-secret-502"},
		{name: "http-503", status: http.StatusServiceUnavailable, body: "phase7i-upstream-secret-503", wantStatus: http.StatusBadGateway, wantCode: upstreamSessionRejectedCode, secret: "phase7i-upstream-secret-503"},
		{name: "http-504", status: http.StatusGatewayTimeout, body: "phase7i-upstream-secret-504", wantStatus: http.StatusBadGateway, wantCode: upstreamSessionRejectedCode, secret: "phase7i-upstream-secret-504"},
	}

	for _, path := range paths {
		for _, tc := range cases {
			t.Run(path+"/"+tc.name, func(t *testing.T) {
				var transportAttempts atomic.Int32
				var sessionHits atomic.Int32
				upstream := phase7ISessionUpstream(t, tc.status, tc.body, &sessionHits)
				defer upstream.Close()

				withTempAppConfig(t, phase1DPlaybackConfig(upstream.URL, 1), func(app *App, handler http.Handler) {
					token, info, itemID, _ := phase7BSeedSessionState(t, app, handler)
					beforeProgress := app.WatchStore.GetProgress(info.UserID, itemID)
					if beforeProgress == nil {
						t.Fatal("seed progress missing")
					}
					beforeLease := phase7ISnapshotLease(t, app, info.UserID, "server-a")

					if tc.transport != nil {
						client := app.Upstream.ClientByID("server-a")
						client.httpClient.Transport = phase7BRoundTripFunc(func(*http.Request) (*http.Response, error) {
							transportAttempts.Add(1)
							return nil, tc.transport
						})
					}

					rr := phase7BPostSession(t, handler, path, token, itemID, 900)
					phase7BAssertSessionError(t, rr, tc.wantStatus, tc.wantCode)
					if tc.secret != "" && strings.Contains(rr.Body.String(), tc.secret) {
						t.Fatalf("sensitive upstream detail leaked to client: %s", rr.Body.String())
					}
					if tc.transport != nil && transportAttempts.Load() != 1 {
						t.Fatalf("transport attempts=%d want=1", transportAttempts.Load())
					}
					if tc.transport == nil && sessionHits.Load() != 1 {
						t.Fatalf("upstream session hits=%d want=1", sessionHits.Load())
					}
					if tc.transport != nil && sessionHits.Load() != 0 {
						t.Fatalf("transport-injected failure unexpectedly reached HTTP server: hits=%d", sessionHits.Load())
					}
					phase7IAssertSecretAbsentFromLogs(t, app, tc.secret)
					phase7IAssertLocalStateUnchanged(t, app, info, itemID, beforeProgress, beforeLease)
				})
			})
		}
	}
}
