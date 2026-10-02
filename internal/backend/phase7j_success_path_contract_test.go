package backend

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
)

func phase7JSessionUpstream(t *testing.T, status int, expectedPath string, sessionHits, wrongPathHits *atomic.Int32) *httptest.Server {
	t.Helper()
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodPost && r.URL.Path == "/Users/AuthenticateByName":
			_ = json.NewEncoder(w).Encode(map[string]any{
				"AccessToken": "tok-a",
				"User":        map[string]any{"Id": "user-a"},
			})
		case r.Method == http.MethodPost && r.URL.Path == expectedPath:
			sessionHits.Add(1)
			w.Header().Set("Content-Type", "text/plain")
			w.WriteHeader(status)
			_, _ = w.Write([]byte("phase7j-upstream-success-body-must-not-propagate"))
		case r.Method == http.MethodPost && (r.URL.Path == "/Sessions/Playing" || r.URL.Path == "/Sessions/Playing/Progress"):
			wrongPathHits.Add(1)
			http.NotFound(w, r)
		default:
			http.NotFound(w, r)
		}
	}))
}

func TestPhase7JPlayingAndProgressSuccessCommitMatrix(t *testing.T) {
	paths := []string{"/Sessions/Playing", "/Sessions/Playing/Progress"}
	statuses := []int{http.StatusOK, http.StatusNoContent}

	for _, path := range paths {
		for _, status := range statuses {
			t.Run(path+"/status-"+http.StatusText(status), func(t *testing.T) {
				var sessionHits atomic.Int32
				var wrongPathHits atomic.Int32
				upstream := phase7JSessionUpstream(t, status, path, &sessionHits, &wrongPathHits)
				defer upstream.Close()

				withTempAppConfig(t, phase1DPlaybackConfig(upstream.URL, 1), func(app *App, handler http.Handler) {
					token, info, itemID, _ := phase7BSeedSessionState(t, app, handler)
					beforeProgress := app.WatchStore.GetProgress(info.UserID, itemID)
					if beforeProgress == nil {
						t.Fatal("seed progress missing")
					}
					if err := app.WatchStore.RecordProgress(&WatchProgress{
						ProxyUserID:    info.UserID,
						VirtualItemID:  itemID,
						ServerID:       "server-a",
						OriginalItemID: "item-a",
						PositionTicks:  beforeProgress.PositionTicks,
						RuntimeTicks:   beforeProgress.RuntimeTicks,
						LastPlayed:     1,
						UpdatedAt:      1,
					}); err != nil {
						t.Fatalf("stabilize seed watch timestamps: %v", err)
					}
					beforeProgress = app.WatchStore.GetProgress(info.UserID, itemID)
					if beforeProgress == nil {
						t.Fatal("stabilized seed progress missing")
					}
					beforeLease := phase7ISnapshotLease(t, app, info.UserID, "server-a")
					if beforeLease.PlaySessionID != "play-a" {
						t.Fatalf("seed PlaySessionID=%q want=play-a", beforeLease.PlaySessionID)
					}
					if beforeLease.LastHeartbeat == 0 {
						t.Fatal("seed heartbeat missing")
					}
					if beforeLease.DeviceID != "xbox-001" || beforeLease.ItemID != itemID {
						t.Fatalf("unexpected seed lease identity: %+v", beforeLease)
					}
					if beforeLease.Revision == 0 {
						t.Fatal("seed lease revision missing")
					}
					otherUserID := info.UserID + "-other"
					otherItemID := app.IDStore.GetOrCreateVirtualID("item-other", "server-a")
					if err := app.WatchStore.RecordProgress(&WatchProgress{ProxyUserID: otherUserID, VirtualItemID: otherItemID, ServerID: "server-a", OriginalItemID: "item-other", PositionTicks: 222, RuntimeTicks: 1000, LastPlayed: 2, UpdatedAt: 2}); err != nil {
						t.Fatalf("seed other-user progress: %v", err)
					}
					otherUserBefore := app.WatchStore.GetProgress(otherUserID, otherItemID)
					if otherUserBefore == nil {
						t.Fatal("other-user seed progress missing")
					}
					if otherUserBefore.PositionTicks != 222 {
						t.Fatalf("other-user seed PositionTicks=%d want=222", otherUserBefore.PositionTicks)
					}
					otherLeaseResult := app.PlaybackLimiter.Reserve(otherUserID, "server-a", "phone-other", otherItemID, "play-other")
					if !otherLeaseResult.Allowed || !otherLeaseResult.Created {
						t.Fatalf("seed other-user lease=%+v", otherLeaseResult)
					}
					otherLeaseBefore := phase7ISnapshotLease(t, app, otherUserID, "server-a")
					if otherLeaseBefore.DeviceID != "phone-other" || otherLeaseBefore.PlaySessionID != "play-other" {
						t.Fatalf("unexpected other-user seed lease: %+v", otherLeaseBefore)
					}
					if count := app.PlaybackLimiter.CountForServer("server-a"); count != 2 {
						t.Fatalf("seed lease count=%d want=2", count)
					}
					rr := phase7BPostSession(t, handler, path, token, itemID, 900)
					if rr.Code != http.StatusNoContent {
						t.Fatalf("status=%d want=%d body=%s", rr.Code, http.StatusNoContent, rr.Body.String())
					}
					if rr.Body.Len() != 0 {
						t.Fatalf("success response leaked upstream body: %q", rr.Body.String())
					}
					if sessionHits.Load() != 1 {
						t.Fatalf("upstream session hits=%d want=1", sessionHits.Load())
					}
					if wrongPathHits.Load() != 0 {
						t.Fatalf("unexpected upstream path hits=%d", wrongPathHits.Load())
					}

					afterProgress := app.WatchStore.GetProgress(info.UserID, itemID)
					if afterProgress == nil {
						t.Fatal("progress missing after successful upstream confirmation")
					}
					if afterProgress.PositionTicks != 900 {
						t.Fatalf("PositionTicks=%d want=900", afterProgress.PositionTicks)
					}
					if beforeProgress.PositionTicks != 111 {
						t.Fatalf("seed PositionTicks=%d want=111", beforeProgress.PositionTicks)
					}
					if beforeProgress.ServerID != "server-a" {
						t.Fatalf("seed ServerID=%q want=server-a", beforeProgress.ServerID)
					}
					if beforeProgress.OriginalItemID != "item-a" {
						t.Fatalf("seed OriginalItemID=%q want=item-a", beforeProgress.OriginalItemID)
					}
					if afterProgress.Played != beforeProgress.Played {
						t.Fatalf("Playing/Progress success unexpectedly changed played state: before=%v after=%v", beforeProgress.Played, afterProgress.Played)
					}
					if beforeProgress.LastPlayed != 1 || beforeProgress.UpdatedAt != 1 {
						t.Fatalf("seed timestamps not stabilized: %#v", beforeProgress)
					}
					if afterProgress.LastPlayed <= beforeProgress.LastPlayed || afterProgress.UpdatedAt <= beforeProgress.UpdatedAt {
						t.Fatalf("successful upstream confirmation did not advance watch timestamps: before=%#v after=%#v", beforeProgress, afterProgress)
					}
					if afterProgress.RuntimeTicks != 1000 {
						t.Fatalf("RuntimeTicks=%d want=1000", afterProgress.RuntimeTicks)
					}
					if beforeProgress.RuntimeTicks != 1000 {
						t.Fatalf("seed RuntimeTicks=%d want=1000", beforeProgress.RuntimeTicks)
					}
					if afterProgress.IsFavorite != beforeProgress.IsFavorite {
						t.Fatalf("success path unexpectedly changed favorite state: before=%v after=%v", beforeProgress.IsFavorite, afterProgress.IsFavorite)
					}
					if afterProgress.ServerID != beforeProgress.ServerID || afterProgress.OriginalItemID != beforeProgress.OriginalItemID {
						t.Fatalf("success path changed item ownership: before=%#v after=%#v", beforeProgress, afterProgress)
					}
					if afterProgress.VirtualItemID != itemID || afterProgress.ProxyUserID != info.UserID {
						t.Fatalf("success path committed progress to wrong owner/item: %#v", afterProgress)
					}
					afterLease := phase7ISnapshotLease(t, app, info.UserID, "server-a")
					if count := app.PlaybackLimiter.CountForServer("server-a"); count != 2 {
						t.Fatalf("successful session changed lease count=%d want=2", count)
					}
					if afterLease.DeviceID != beforeLease.DeviceID || afterLease.ItemID != beforeLease.ItemID || afterLease.PlaySessionID != beforeLease.PlaySessionID {
						t.Fatalf("successful heartbeat changed lease identity: before=%+v after=%+v", beforeLease, afterLease)
					}
					if afterLease.Revision != beforeLease.Revision {
						t.Fatalf("heartbeat unexpectedly changed lease revision: before=%d after=%d", beforeLease.Revision, afterLease.Revision)
					}
					if afterLease.LastHeartbeat <= beforeLease.LastHeartbeat {
						t.Fatalf("successful upstream confirmation did not advance heartbeat: before=%d after=%d", beforeLease.LastHeartbeat, afterLease.LastHeartbeat)
					}

					otherUserAfter := app.WatchStore.GetProgress(otherUserID, otherItemID)
					if otherUserAfter == nil || otherUserAfter.PositionTicks != otherUserBefore.PositionTicks || otherUserAfter.RuntimeTicks != otherUserBefore.RuntimeTicks || otherUserAfter.LastPlayed != otherUserBefore.LastPlayed || otherUserAfter.UpdatedAt != otherUserBefore.UpdatedAt {
						t.Fatalf("success path mutated other-user watch state: before=%#v after=%#v", otherUserBefore, otherUserAfter)
					}
					if otherUserAfter.Played != otherUserBefore.Played || otherUserAfter.IsFavorite != otherUserBefore.IsFavorite || otherUserAfter.ServerID != otherUserBefore.ServerID || otherUserAfter.OriginalItemID != otherUserBefore.OriginalItemID {
						t.Fatalf("success path mutated other-user metadata/state: before=%#v after=%#v", otherUserBefore, otherUserAfter)
					}
					otherLeaseAfter := phase7ISnapshotLease(t, app, otherUserID, "server-a")
					if otherLeaseAfter != otherLeaseBefore {
						t.Fatalf("success path mutated other-user lease: before=%+v after=%+v", otherLeaseBefore, otherLeaseAfter)
					}
				})
			})
		}
	}
}
