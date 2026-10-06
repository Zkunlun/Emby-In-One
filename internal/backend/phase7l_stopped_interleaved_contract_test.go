package backend

import (
	"net/http"
	"net/http/httptest"
	"strconv"
	"sync"
	"testing"
	"time"
)

func phase7LStartGatedStopped(t *testing.T, f *phase7KFixture, status int, route string) (*httptest.ResponseRecorder, <-chan struct{}, func()) {
	t.Helper()
	entered, release := make(chan struct{}, 1), make(chan struct{})
	var once sync.Once
	unblock := func() { once.Do(func() { close(release) }) }
	f.Upstreams[0].mu.Lock()
	f.Upstreams[0].hook = func(r *http.Request, body map[string]any, defaultStatus int) int {
		position, _ := numericInt64(body["PositionTicks"])
		if r.URL.Path != phase7LStoppedPath || position != 900 {
			return defaultStatus
		}
		select {
		case entered <- struct{}{}:
		case <-r.Context().Done():
			return defaultStatus
		}
		select {
		case <-release:
			return status
		case <-r.Context().Done():
			return defaultStatus
		}
	}
	f.Upstreams[0].mu.Unlock()
	body := f.Body(0, 0, 900)
	if route == "active" {
		delete(body, "MediaSourceId")
	}
	rr, done, cancel := phase7KStartSession(t, f, phase7LStoppedPath, body)
	t.Cleanup(func() {
		unblock()
		cancel()
		select {
		case <-done:
		case <-time.After(5 * time.Second):
			t.Error("gated Stopped did not terminate during cleanup")
		}
	})
	select {
	case <-entered:
	case <-done:
		t.Fatalf("Stopped completed before gate: status=%d", rr.Code)
	case <-time.After(5 * time.Second):
		t.Fatal("Stopped failed to reach upstream gate")
	}
	return rr, done, unblock
}

func phase7LAssertPending(t *testing.T, done <-chan struct{}) {
	t.Helper()
	select {
	case <-done:
		t.Fatal("Stopped completed before upstream gate was released")
	default:
	}
}

func phase7LWaitStopped(t *testing.T, done <-chan struct{}) {
	t.Helper()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("Stopped did not complete after gate was released")
	}
}

func TestPhase7LDelayedStoppedKeepsNewSessionAndTakeoverLease(t *testing.T) {
	for _, status := range []int{http.StatusNoContent, http.StatusInternalServerError} {
		for _, route := range []string{"active", "media"} {
			for _, replacement := range []string{"same device new session", "other device takeover"} {
				t.Run(strconv.Itoa(status)+"/"+route+"/"+replacement, func(t *testing.T) {
					phase7KWithFixture(t, func(f *phase7KFixture) {
						before := phase7LSnapshot(t, f)
						rr, done, unblock := phase7LStartGatedStopped(t, f, status, route)
						phase7LAssertUnchangedExcept(t, f, before, nil, nil)
						key := f.Key(0, 0)
						device := phase7KDeviceID
						if replacement == "other device takeover" {
							device = "phase7l-takeover-device"
							phase1EAgeLease(t, f.App, key.UserID, key.ServerID, time.Now().Add(-playbackHeartbeatTimeout-time.Second))
						}
						session := "phase7l-new-session"
						virtualSession := f.App.IDStore.GetOrCreateVirtualID(session, key.ServerID)
						result := f.App.PlaybackLimiter.Reserve(key.UserID, key.ServerID, device, f.Items[0], session)
						if !result.Allowed || result.Revision <= before.Local.Leases[key].Revision {
							t.Fatalf("new lease reservation=%+v", result)
						}
						if result.Created != (replacement == "other device takeover") {
							t.Fatalf("replacement kind Created=%v", result.Created)
						}
						owner := "token:" + f.Users[0].Token
						f.App.playbackRoutes.Activate(owner, f.Items[0], key.ServerID, session, virtualSession)
						f.App.playbackRoutes.RememberMediaSource(owner, f.Media[0], key.ServerID, session, virtualSession)
						seedPhase5WatchOwner(t, f.App, key.UserID, f.Items[0], key.ServerID, phase7KItemID, device, session, "shared-media", 2000)
						beforeProgress := phase7LSnapshot(t, f)
						progressBody := f.Body(0, 0, 800)
						progressBody["PlaySessionId"] = virtualSession
						progress := phase1ESessionPost(t, f.Handler, "/Sessions/Playing/Progress", f.Users[0].Token, device, progressBody)
						phase7KAssertEmptySuccess(t, progress)
						watchKey := phase7LWatchOwner(f, 0, 0)
						phase7LAssertProgress(t, f, beforeProgress, watchKey, key.ServerID, phase7KItemID, 800, 2000, false)
						f.AssertCommitted(t, beforeProgress.Local, 0, 0, 800, false)
						phase7LAssertUnchangedExcept(t, f, beforeProgress, []phase7LWatchKey{watchKey}, []streamKey{key})
						phase7LAssertPending(t, done)
						newCommitted := phase7LSnapshot(t, f)
						unblock()
						phase7LWaitStopped(t, done)
						phase7KAssertEmptySuccess(t, rr)
						// Task 4 fences the old terminal event after the new Started owner.
						phase7LAssertUnchangedExcept(t, f, newCommitted, nil, nil)
						requests := f.Upstreams[0].Requests()
						if len(requests) != 2 || len(f.Upstreams[1].Requests()) != 0 ||
							requests[0].Path != phase7LStoppedPath || requests[0].Body["PlaySessionId"] != phase7KSessionID ||
							requests[1].Path != "/Sessions/Playing/Progress" || requests[1].Body["PlaySessionId"] != session {
							t.Fatalf("delayed Stopped was rerouted onto a new session: %+v", requests)
						}
					})
				})
			}
		}
	}
}

func TestPhase7LPendingStoppedCannotFinalizeOtherUserOrUpstream(t *testing.T) {
	for _, status := range []int{http.StatusNoContent, http.StatusInternalServerError} {
		for _, route := range []string{"active", "media"} {
			for _, other := range []struct {
				name         string
				user, server int
			}{
				{name: "other user same upstream", user: 1},
				{name: "same user other upstream", server: 1},
				{name: "other user other upstream", user: 1, server: 1},
			} {
				t.Run(strconv.Itoa(status)+"/"+route+"/"+other.name, func(t *testing.T) {
					phase7KWithFixture(t, func(f *phase7KFixture) {
						before := phase7LSnapshot(t, f)
						rr, done, unblock := phase7LStartGatedStopped(t, f, status, route)
						phase7LAssertUnchangedExcept(t, f, before, nil, nil)
						otherResponse := phase1ESessionPost(t, f.Handler, phase7LStoppedPath, f.Users[other.user].Token, phase7KDeviceID, f.Body(other.user, other.server, 700))
						phase7KAssertEmptySuccess(t, otherResponse)
						otherWatch := phase7LWatchOwner(f, other.user, other.server)
						otherLease := f.Key(other.user, other.server)
						phase7LAssertProgress(t, f, before, otherWatch, otherLease.ServerID, phase7KItemID, 700, 2000, false)
						phase7LAssertLeaseAbsent(t, f, otherLease)
						phase7LAssertUnchangedExcept(t, f, before, []phase7LWatchKey{otherWatch}, []streamKey{otherLease})
						phase7LAssertPending(t, done)
						otherCommitted := phase7LSnapshot(t, f)
						unblock()
						phase7LWaitStopped(t, done)
						phase7KAssertEmptySuccess(t, rr)
						targetWatch, targetLease := phase7LWatchOwner(f, 0, 0), f.Key(0, 0)
						phase7LAssertProgress(t, f, otherCommitted, targetWatch, targetLease.ServerID, phase7KItemID, 900, 2000, false)
						phase7LAssertLeaseAbsent(t, f, targetLease)
						phase7LAssertUnchangedExcept(t, f, otherCommitted, []phase7LWatchKey{targetWatch}, []streamKey{targetLease})
						wantFirst, wantSecond := 1, 1
						if other.server == 0 {
							wantFirst, wantSecond = 2, 0
						}
						if len(f.Upstreams[0].Requests()) != wantFirst || len(f.Upstreams[1].Requests()) != wantSecond {
							t.Fatal("interleaved Stopped reached an unrelated upstream")
						}
						for server, upstream := range f.Upstreams {
							for _, sent := range upstream.Requests() {
								if sent.Path != phase7LStoppedPath || sent.Body["PlaySessionId"] != phase7KSessionID || sent.Body["UserId"] != "upstream-user-"+f.Servers[server] {
									t.Fatalf("interleaved Stopped identity mismatch: %+v", sent)
								}
							}
						}
					})
				})
			}
		}
	}
}
