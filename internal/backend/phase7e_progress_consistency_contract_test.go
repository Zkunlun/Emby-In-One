package backend

import (
	"context"
	"errors"
	"net/http"
	"testing"
)

func TestPhase7EProgressCommitsOnlyAfterUpstreamConfirmation(t *testing.T) {
	for _, tc := range []struct {
		name       string
		status     int
		body       string
		transport  error
		offline    bool
		wantStatus int
		wantCode   string
		wantCommit bool
	}{
		{name: "confirmed 2xx", status: http.StatusOK, body: "OK", wantStatus: http.StatusNoContent, wantCommit: true},
		{name: "upstream rejected", status: http.StatusInternalServerError, body: "sensitive-upstream-detail", wantStatus: http.StatusBadGateway, wantCode: upstreamSessionRejectedCode},
		{name: "transport failure", status: http.StatusNoContent, transport: errors.New("dial tcp secret.example:443: refused"), wantStatus: http.StatusBadGateway, wantCode: upstreamSessionFailedCode},
		{name: "timeout", status: http.StatusNoContent, transport: context.DeadlineExceeded, wantStatus: http.StatusGatewayTimeout, wantCode: upstreamSessionTimeoutCode},
		{name: "canceled", status: http.StatusNoContent, transport: context.Canceled, wantStatus: http.StatusBadGateway, wantCode: upstreamSessionFailedCode},
		{name: "offline", status: http.StatusNoContent, offline: true, wantStatus: http.StatusServiceUnavailable, wantCode: upstreamSessionUnavailableCode},
	} {
		t.Run(tc.name, func(t *testing.T) {
			upstream := phase7BSessionUpstream(t, tc.status, tc.body)
			defer upstream.Close()

			withTempAppConfig(t, phase1DPlaybackConfig(upstream.URL, 1), func(app *App, handler http.Handler) {
				token, info, itemID, before := phase7BSeedSessionState(t, app, handler)
				client := app.Upstream.ClientByID("server-a")
				if tc.transport != nil {
					client.httpClient.Transport = phase7BRoundTripFunc(func(*http.Request) (*http.Response, error) {
						return nil, tc.transport
					})
				}
				if tc.offline {
					client.setOffline("phase7e offline")
				}

				rr := phase7BPostSession(t, handler, "/Sessions/Playing/Progress", token, itemID, 900)
				if rr.Code != tc.wantStatus {
					t.Fatalf("status=%d want=%d body=%s", rr.Code, tc.wantStatus, rr.Body.String())
				}
				if tc.wantCode != "" && phase1GErrorCode(t, rr) != tc.wantCode {
					t.Fatalf("body=%s want code=%s", rr.Body.String(), tc.wantCode)
				}

				progress := app.WatchStore.GetProgress(info.UserID, itemID)
				if tc.wantCommit {
					if progress == nil || !progress.Played || progress.PositionTicks != 0 || progress.RuntimeTicks != 1000 {
						t.Fatalf("confirmed Progress did not commit progress: %#v", progress)
					}
					after := phase1ELeaseHeartbeat(t, app, info.UserID, "server-a")
					if !after.After(before) {
						t.Fatalf("confirmed Progress did not refresh heartbeat: before=%v after=%v", before, after)
					}
					return
				}
				phase7BAssertNoLocalCommit(t, app, info, itemID, before)
			})
		})
	}
}
