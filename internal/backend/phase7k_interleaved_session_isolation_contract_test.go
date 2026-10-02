package backend

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strconv"
	"sync"
	"testing"
	"time"
)

func phase7KStartSession(t *testing.T, f *phase7KFixture, path string, body map[string]any) (*httptest.ResponseRecorder, <-chan struct{}, context.CancelFunc) {
	t.Helper()
	encoded, err := json.Marshal(body)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	req := httptest.NewRequest(http.MethodPost, path, bytes.NewReader(encoded)).WithContext(ctx)
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-Emby-Token", f.Users[0].Token)
	req.Header.Set("X-Emby-Device-Id", phase7KDeviceID)
	rr := httptest.NewRecorder()
	done := make(chan struct{})
	go func() {
		defer close(done)
		f.Handler.ServeHTTP(rr, req)
	}()
	return rr, done, cancel
}

func TestPhase7KInterleavedConfirmationIsolatedByUserAndUpstream(t *testing.T) {
	for _, path := range []string{"/Sessions/Playing", "/Sessions/Playing/Progress"} {
		for _, targetStatus := range []int{http.StatusNoContent, http.StatusInternalServerError} {
			for _, other := range []struct {
				name         string
				user, server int
			}{
				{name: "other user same upstream", user: 1, server: 0},
				{name: "same user other upstream", user: 0, server: 1},
				{name: "other user other upstream", user: 1, server: 1},
			} {
				t.Run(path+"/"+strconv.Itoa(targetStatus)+"/"+other.name, func(t *testing.T) {
					phase7KWithFixture(t, func(f *phase7KFixture) {
						entered, release := make(chan struct{}, 1), make(chan struct{})
						var once sync.Once
						unblock := func() { once.Do(func() { close(release) }) }
						defer unblock()
						f.Upstreams[0].mu.Lock()
						f.Upstreams[0].hook = func(r *http.Request, body map[string]any, defaultStatus int) int {
							position, _ := numericInt64(body["PositionTicks"])
							if position != 900 {
								return defaultStatus
							}
							select {
							case entered <- struct{}{}:
							case <-r.Context().Done():
								return defaultStatus
							}
							select {
							case <-release:
								return targetStatus
							case <-r.Context().Done():
								return defaultStatus
							}
						}
						f.Upstreams[0].mu.Unlock()
						before := f.Snapshot(t)
						rr, done, cancel := phase7KStartSession(t, f, path, f.Body(0, 0, 900))
						defer func() {
							unblock()
							cancel()
							select {
							case <-done:
							case <-time.After(5 * time.Second):
								t.Error("blocked handler did not terminate during cleanup")
							}
						}()
						select {
						case <-entered:
						case <-done:
							t.Fatalf("target returned without reaching blocked upstream: status=%d", rr.Code)
						case <-time.After(5 * time.Second):
							t.Fatal("target did not reach blocked upstream")
						}
						f.AssertUnchangedExcept(t, before)

						otherResponse := phase1ESessionPost(t, f.Handler, path, f.Users[other.user].Token, phase7KDeviceID, f.Body(other.user, other.server, 700))
						phase7KAssertEmptySuccess(t, otherResponse)
						f.AssertCommitted(t, before, other.user, other.server, 700, false)
						f.AssertUnchangedExcept(t, before, f.Key(other.user, other.server))
						// The independent request must not manufacture confirmation for
						// the still-pending target, even when identifiers are identical.
						select {
						case <-done:
							t.Fatal("pending target completed before its upstream confirmation")
						default:
						}
						otherCommitted := f.Snapshot(t)
						unblock()
						select {
						case <-done:
						case <-time.After(5 * time.Second):
							t.Fatal("target failed to terminate after release")
						}
						if targetStatus == http.StatusNoContent {
							phase7KAssertEmptySuccess(t, rr)
							f.AssertCommitted(t, before, 0, 0, 900, false)
							f.AssertUnchangedExcept(t, otherCommitted, f.Key(0, 0))
						} else {
							phase7BAssertSessionError(t, rr, http.StatusBadGateway, upstreamSessionRejectedCode)
							f.AssertUnchangedExcept(t, otherCommitted)
						}
						wantHits := 1
						if other.server == 0 {
							wantHits = 2
						}
						if len(f.Upstreams[0].Requests()) != wantHits {
							t.Fatalf("blocked upstream request count=%d want=%d", len(f.Upstreams[0].Requests()), wantHits)
						}
					})
				})
			}
		}
	}
}
