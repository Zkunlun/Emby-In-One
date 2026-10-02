package backend

import (
	"context"
	"errors"
	"net/http"
	"strconv"
	"sync/atomic"
	"testing"
)

func TestPhase7KSuccessChangesOnlyAuthenticatedUserAndTargetUpstream(t *testing.T) {
	for _, path := range []string{"/Sessions/Playing", "/Sessions/Playing/Progress"} {
		for userIndex := 0; userIndex < 2; userIndex++ {
			for serverIndex := 0; serverIndex < 2; serverIndex++ {
				t.Run(path+"/user-"+strconv.Itoa(userIndex)+"/server-"+strconv.Itoa(serverIndex), func(t *testing.T) {
					phase7KWithFixture(t, func(f *phase7KFixture) {
						before := f.Snapshot(t)
						body := f.Body(userIndex, serverIndex, 700)
						// A body UserId never selects the owner of local state. The
						// outbound field is normalized to this upstream's shared account.
						body["UserId"] = f.Users[1-userIndex].Info.UserID
						rr := phase1ESessionPost(t, f.Handler, path, f.Users[userIndex].Token, phase7KDeviceID, body)
						phase7KAssertEmptySuccess(t, rr)
						f.AssertCommitted(t, before, userIndex, serverIndex, 700, false)
						f.AssertUnchangedExcept(t, before, f.Key(userIndex, serverIndex))
						f.AssertForwarded(t, serverIndex, path, 700)
						if len(f.Upstreams[serverIndex].Requests()) != 1 || len(f.Upstreams[1-serverIndex].Requests()) != 0 {
							t.Fatal("session request reached the wrong upstream or was duplicated")
						}
						for _, serverID := range f.Servers {
							if count := f.App.PlaybackLimiter.CountForServer(serverID); count != 2 {
								t.Fatalf("success changed %s lease count=%d want=2", serverID, count)
							}
						}
					})
				})
			}
		}
	}
}

func TestPhase7KFailedSessionCannotCommitAnyUserOrUpstreamState(t *testing.T) {
	for _, path := range []string{"/Sessions/Playing", "/Sessions/Playing/Progress"} {
		for caseIndex, tc := range []struct {
			name       string
			wantStatus int
			wantCode   string
			transport  error
		}{
			{name: "HTTP rejection", wantStatus: http.StatusBadGateway, wantCode: upstreamSessionRejectedCode},
			{name: "transport", transport: errors.New("isolation transport failure"), wantStatus: http.StatusBadGateway, wantCode: upstreamSessionFailedCode},
			{name: "deadline", transport: context.DeadlineExceeded, wantStatus: http.StatusGatewayTimeout, wantCode: upstreamSessionTimeoutCode},
			{name: "net timeout", transport: phase7ITimeoutError{}, wantStatus: http.StatusGatewayTimeout, wantCode: upstreamSessionTimeoutCode},
			{name: "canceled", transport: context.Canceled, wantStatus: http.StatusBadGateway, wantCode: upstreamSessionFailedCode},
			{name: "offline", wantStatus: http.StatusServiceUnavailable, wantCode: upstreamSessionUnavailableCode},
			{name: "missing", wantStatus: http.StatusServiceUnavailable, wantCode: upstreamSessionUnavailableCode},
			{name: "auth preparation", wantStatus: http.StatusServiceUnavailable},
			{name: "URL preparation", wantStatus: http.StatusBadRequest},
		} {
			t.Run(path+"/"+tc.name, func(t *testing.T) {
				phase7KWithFixture(t, func(f *phase7KFixture) {
					userIndex, serverIndex := caseIndex%2, (caseIndex/2)%2
					before := f.Snapshot(t)
					client := f.App.Upstream.ClientByID(f.Servers[serverIndex])
					var attempts atomic.Int32
					if tc.transport != nil {
						client.httpClient.Transport = phase7BRoundTripFunc(func(*http.Request) (*http.Response, error) {
							attempts.Add(1)
							return nil, tc.transport
						})
					}
					switch tc.name {
					case "HTTP rejection":
						f.Upstreams[serverIndex].mu.Lock()
						f.Upstreams[serverIndex].status = http.StatusInternalServerError
						f.Upstreams[serverIndex].mu.Unlock()
					case "offline":
						client.setOffline("isolation fixture offline")
					case "missing":
						phase7FRemoveUpstreamClient(f.App, f.Servers[serverIndex])
					case "auth preparation":
						phase7HClearUpstreamUserIdentity(client)
					case "URL preparation":
						phase7HCorruptUpstreamBaseURL(client)
					}
					rr := phase1ESessionPost(t, f.Handler, path, f.Users[userIndex].Token, phase7KDeviceID, f.Body(userIndex, serverIndex, 700))
					if tc.name == "auth preparation" {
						phase7HAssertPreparationError(t, rr)
					} else if tc.name == "URL preparation" {
						phase7HAssertClientInputPreparationError(t, rr)
					} else {
						phase7BAssertSessionError(t, rr, tc.wantStatus, tc.wantCode)
					}
					f.AssertUnchangedExcept(t, before)
					wantHits := 0
					if tc.name == "HTTP rejection" {
						wantHits = 1
					}
					if len(f.Upstreams[serverIndex].Requests()) != wantHits || len(f.Upstreams[1-serverIndex].Requests()) != 0 {
						t.Fatal("failure was forwarded to an unrelated upstream or unexpectedly reached HTTP")
					}
					if tc.transport != nil && attempts.Load() != 1 {
						t.Fatalf("transport attempts=%d want=1", attempts.Load())
					}
				})
			})
		}
	}
}
