package backend

import (
	"net/http"
	"strconv"
	"sync/atomic"
	"testing"
)

func TestPhase7JAdditional2xxCommitsWithoutChangingLeaseOwnership(t *testing.T) {
	for _, path := range []string{"/Sessions/Playing", "/Sessions/Playing/Progress"} {
		for _, status := range []int{http.StatusCreated, http.StatusAccepted, http.StatusPartialContent, 299} {
			t.Run(path+"/status-"+strconv.Itoa(status), func(t *testing.T) {
				var sessionHits, wrongPathHits atomic.Int32
				upstream := phase7JSessionUpstream(t, status, path, &sessionHits, &wrongPathHits)
				defer upstream.Close()
				withTempAppConfig(t, phase1DPlaybackConfig(upstream.URL, 1), func(app *App, handler http.Handler) {
					token, info, itemID, _ := phase7BSeedSessionState(t, app, handler)
					if err := app.WatchStore.RecordProgress(&WatchProgress{
						ProxyUserID: info.UserID, VirtualItemID: itemID,
						ServerID: "server-a", OriginalItemID: "item-a", ItemType: "Movie",
						PositionTicks: 111, RuntimeTicks: 1000, LastPlayed: 1, UpdatedAt: 1,
					}); err != nil {
						t.Fatalf("seed progress: %v", err)
					}
					before := phase7ISnapshotLease(t, app, info.UserID, "server-a")
					rr := phase1ESessionPost(t, handler, path, token, "xbox-001", map[string]any{
						"ItemId": itemID, "PositionTicks": int64(222), "RunTimeTicks": int64(2000),
						"PlaySessionId": "play-a",
					})
					if rr.Code != http.StatusNoContent || rr.Body.Len() != 0 {
						t.Fatalf("response status=%d body=%q want empty 204", rr.Code, rr.Body.String())
					}
					if sessionHits.Load() != 1 || wrongPathHits.Load() != 0 {
						t.Fatalf("session hits=%d wrong path hits=%d want 1/0", sessionHits.Load(), wrongPathHits.Load())
					}
					progress := app.WatchStore.GetProgress(info.UserID, itemID)
					if progress == nil || progress.PositionTicks != 222 || progress.RuntimeTicks != 2000 ||
						progress.LastPlayed <= 1 || progress.UpdatedAt <= 1 {
						t.Fatalf("confirmed 2xx did not commit full playback progress: %#v", progress)
					}
					after := phase7ISnapshotLease(t, app, info.UserID, "server-a")
					if after.DeviceID != before.DeviceID || after.ItemID != before.ItemID ||
						after.PlaySessionID != before.PlaySessionID || after.Revision != before.Revision ||
						after.LastHeartbeat <= before.LastHeartbeat {
						t.Fatalf("heartbeat changed ownership or failed to advance: before=%+v after=%+v", before, after)
					}
					if count := app.PlaybackLimiter.CountForServer("server-a"); count != 1 {
						t.Fatalf("success changed lease count=%d want=1", count)
					}
				})
			})
		}
	}
}
