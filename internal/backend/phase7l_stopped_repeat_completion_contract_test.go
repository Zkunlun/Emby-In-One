package backend

import (
	"io"
	"net/http"
	"strconv"
	"sync/atomic"
	"testing"
)

func TestPhase7LRepeatedStoppedCannotRecreateOrReleaseLaterLease(t *testing.T) {
	for _, mode := range phase7LModes() {
		for _, replacement := range []string{"none", "same device new session", "other device new session"} {
			t.Run(mode.Name+"/"+replacement, func(t *testing.T) {
				phase7KWithFixture(t, func(f *phase7KFixture) {
					before := phase7LSnapshot(t, f)
					attempts := phase7LPrepareMode(t, f, 0, mode)
					body := f.Body(0, 0, 700)
					first := phase1ESessionPost(t, f.Handler, phase7LStoppedPath, f.Users[0].Token, phase7KDeviceID, body)
					phase7LAssertResponse(t, first, mode)
					key := phase7LWatchOwner(f, 0, 0)
					phase7LAssertProgress(t, f, before, key, "server-a", phase7KItemID, 700, 2000, false)
					phase7LAssertLeaseAbsent(t, f, f.Key(0, 0))
					phase7LAssertUnchangedExcept(t, f, before, []phase7LWatchKey{key}, []streamKey{f.Key(0, 0)})
					if replacement != "none" {
						device := phase7KDeviceID
						if replacement == "other device new session" {
							device = "phase7l-new-device"
						}
						session := "phase7l-new-session"
						virtualSession := f.App.IDStore.GetOrCreateVirtualID(session, "server-a")
						result := f.App.PlaybackLimiter.Reserve(f.Users[0].Info.UserID, "server-a", device, f.Items[0], session)
						if !result.Allowed || !result.Created {
							t.Fatalf("replacement reserve=%+v", result)
						}
						owner := "token:" + f.Users[0].Token
						f.App.playbackRoutes.Activate(owner, f.Items[0], "server-a", session, virtualSession)
						f.App.playbackRoutes.RememberMediaSource(owner, f.Media[0], "server-a", session, virtualSession)
					}
					beforeRepeat := phase7LSnapshot(t, f)
					repeated := phase1ESessionPost(t, f.Handler, phase7LStoppedPath, f.Users[0].Token, phase7KDeviceID, f.Body(0, 0, 700))
					phase7LAssertResponse(t, repeated, mode)
					phase7LAssertProgress(t, f, beforeRepeat, key, "server-a", phase7KItemID, 700, 2000, false)
					phase7LAssertUnchangedExcept(t, f, beforeRepeat, []phase7LWatchKey{key}, nil)
					if replacement == "none" {
						phase7LAssertLeaseAbsent(t, f, f.Key(0, 0))
					}
					wantHits, wantAttempts := 0, int32(0)
					if mode.Status != 0 {
						wantHits = 2
					}
					if mode.Transport != nil {
						wantAttempts = 2
					}
					requests := f.Upstreams[0].Requests()
					if len(requests) != wantHits || len(f.Upstreams[1].Requests()) != 0 || attempts.Load() != wantAttempts {
						t.Fatal("duplicate Stopped upstream count changed")
					}
					for _, sent := range requests {
						if sent.Path != phase7LStoppedPath || sent.Body["PlaySessionId"] != phase7KSessionID {
							t.Fatalf("duplicate Stopped acquired replacement session: %+v", sent)
						}
					}
				})
			})
		}
	}
}

func TestPhase7LStoppedCompletionThresholdAndUnknownRuntime(t *testing.T) {
	for caseIndex, tc := range []struct {
		name                                         string
		position, runtime, wantPosition, wantRuntime int64
		omitRuntime, played                          bool
	}{
		{name: "below 90 percent", position: 899, runtime: 1000, wantPosition: 899, wantRuntime: 1000},
		{name: "exactly 90 percent", position: 900, runtime: 1000, wantRuntime: 1000, played: true},
		{name: "above 90 percent", position: 901, runtime: 1000, wantRuntime: 1000, played: true},
		{name: "at end", position: 1000, runtime: 1000, wantRuntime: 1000, played: true},
		{name: "zero position", runtime: 1000, wantRuntime: 1000},
		{name: "zero runtime", position: 700, wantPosition: 700, wantRuntime: 1000},
		{name: "omitted runtime", position: 700, wantPosition: 700, wantRuntime: 1000, omitRuntime: true},
		{name: "negative runtime", position: 700, runtime: -1, wantPosition: 700, wantRuntime: 1000},
	} {
		for _, mode := range []phase7LMode{{Name: "HTTP 204", Status: http.StatusNoContent}, {Name: "HTTP 500", Status: http.StatusInternalServerError}, {Name: "offline"}, {Name: "auth preparation"}, {Name: "URL preparation"}} {
			t.Run(tc.name+"/"+mode.Name, func(t *testing.T) {
				phase7KWithFixture(t, func(f *phase7KFixture) {
					user, server := caseIndex%2, (caseIndex/2)%2
					body := f.Body(user, server, tc.position)
					body["RunTimeTicks"] = tc.runtime
					if tc.omitRuntime {
						delete(body, "RunTimeTicks")
					}
					before := phase7LSnapshot(t, f)
					attempts := phase7LPrepareMode(t, f, server, mode)
					rr := phase1ESessionPost(t, f.Handler, phase7LStoppedPath, f.Users[user].Token, phase7KDeviceID, body)
					phase7LAssertResponse(t, rr, mode)
					key := phase7LWatchOwner(f, user, server)
					phase7LAssertProgress(t, f, before, key, f.Servers[server], phase7KItemID, tc.wantPosition, tc.wantRuntime, tc.played)
					phase7LAssertLeaseAbsent(t, f, f.Key(user, server))
					phase7LAssertUnchangedExcept(t, f, before, []phase7LWatchKey{key}, []streamKey{f.Key(user, server)})
					// Completion changes local resume position only; the outbound event
					// must retain the actual final position observed by the client.
					phase7LAssertCalls(t, f, server, mode, attempts, phase7KSessionID, tc.position)
				})
			})
		}
	}
}

type phase7LUnreadableResponseBody struct {
	Reads  atomic.Int32
	Closes atomic.Int32
}

func (b *phase7LUnreadableResponseBody) Read([]byte) (int, error) {
	b.Reads.Add(1)
	return 0, io.ErrUnexpectedEOF
}
func (b *phase7LUnreadableResponseBody) Close() error {
	b.Closes.Add(1)
	return nil
}

func TestPhase7LStoppedResponseBodyCannotPreventLocalFinalization(t *testing.T) {
	for _, status := range []int{http.StatusOK, http.StatusNoContent, http.StatusInternalServerError} {
		t.Run(strconv.Itoa(status), func(t *testing.T) {
			phase7KWithFixture(t, func(f *phase7KFixture) {
				body := &phase7LUnreadableResponseBody{}
				var calls atomic.Int32
				f.App.Upstream.ClientByID("server-a").httpClient.Transport = phase7BRoundTripFunc(func(r *http.Request) (*http.Response, error) {
					calls.Add(1)
					if r.Method != http.MethodPost || r.URL.Path != phase7LStoppedPath {
						t.Errorf("unexpected request %s %s", r.Method, r.URL.Path)
					}
					return &http.Response{StatusCode: status, Header: http.Header{}, Body: body}, nil
				})
				before := phase7LSnapshot(t, f)
				rr := phase1ESessionPost(t, f.Handler, phase7LStoppedPath, f.Users[0].Token, phase7KDeviceID, f.Body(0, 0, 700))
				phase7KAssertEmptySuccess(t, rr)
				key := phase7LWatchOwner(f, 0, 0)
				phase7LAssertProgress(t, f, before, key, "server-a", phase7KItemID, 700, 2000, false)
				phase7LAssertLeaseAbsent(t, f, f.Key(0, 0))
				phase7LAssertUnchangedExcept(t, f, before, []phase7LWatchKey{key}, []streamKey{f.Key(0, 0)})
				if calls.Load() != 1 || body.Reads.Load() != 0 || body.Closes.Load() != 1 {
					t.Fatalf("calls=%d body reads=%d closes=%d", calls.Load(), body.Reads.Load(), body.Closes.Load())
				}
				if len(f.Upstreams[0].Requests()) != 0 || len(f.Upstreams[1].Requests()) != 0 {
					t.Fatal("body fixture unexpectedly reached HTTP upstream")
				}
			})
		})
	}
}
