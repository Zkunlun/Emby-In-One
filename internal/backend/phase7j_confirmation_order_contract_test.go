package backend

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strconv"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

type phase7JObservedSession struct {
	body map[string]any
	err  error
}

func TestPhase7JSuccessWaitsForUpstreamConfirmation(t *testing.T) {
	for _, path := range []string{"/Sessions/Playing", "/Sessions/Playing/Progress"} {
		for _, status := range []int{http.StatusOK, http.StatusNoContent} {
			for _, routeKind := range []string{"active", "media"} {
				t.Run(path+"/"+strconv.Itoa(status)+"/"+routeKind, func(t *testing.T) {
					reached := make(chan phase7JObservedSession, 1)
					release := make(chan struct{})
					var releaseOnce sync.Once
					unblock := func() { releaseOnce.Do(func() { close(release) }) }
					var sessionHits atomic.Int32
					upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
						switch {
						case r.Method == http.MethodPost && r.URL.Path == "/Users/AuthenticateByName":
							_ = json.NewEncoder(w).Encode(map[string]any{
								"AccessToken": "tok-a", "User": map[string]any{"Id": "user-a"},
							})
						case r.Method == http.MethodPost && r.URL.Path == path:
							sessionHits.Add(1)
							var body map[string]any
							err := json.NewDecoder(r.Body).Decode(&body)
							select {
							case reached <- phase7JObservedSession{body: body, err: err}:
							case <-r.Context().Done():
								return
							}
							select {
							case <-release:
							case <-r.Context().Done():
								return
							}
							w.WriteHeader(status)
							if status != http.StatusNoContent {
								_, _ = w.Write([]byte("confirmed non-JSON success"))
							}
						default:
							http.NotFound(w, r)
						}
					}))
					defer upstream.Close()
					defer unblock()

					withTempAppConfig(t, phase1DPlaybackConfig(upstream.URL, 1), func(app *App, handler http.Handler) {
						token, info, itemID, _ := phase7BSeedSessionState(t, app, handler)
						if err := app.WatchStore.RecordProgress(&WatchProgress{
							ProxyUserID: info.UserID, VirtualItemID: itemID,
							ServerID: "server-a", OriginalItemID: "item-a", ItemType: "Movie",
							PositionTicks: 111, RuntimeTicks: 1000, IsFavorite: true,
							LastPlayed: 1, UpdatedAt: 1,
						}); err != nil {
							t.Fatalf("seed progress: %v", err)
						}
						beforeProgress := app.WatchStore.GetProgress(info.UserID, itemID)
						if beforeProgress == nil {
							t.Fatal("seed progress missing")
						}
						beforeLease := phase7ISnapshotLease(t, app, info.UserID, "server-a")
						clientSession := app.IDStore.GetOrCreateVirtualID("play-a", "server-a")
						owner := "token:" + token
						body := map[string]any{
							"ItemId": itemID, "UserId": info.UserID, "PlaySessionId": clientSession,
							"PositionTicks": int64(900), "RunTimeTicks": int64(2000),
						}
						if routeKind == "media" {
							mediaID := app.IDStore.GetOrCreateVirtualID("ms-a", "server-a")
							app.playbackRoutes.RememberMediaSource(owner, mediaID, "server-a", "play-a", clientSession)
							body["MediaSourceId"] = mediaID
						} else {
							app.playbackRoutes.Activate(owner, itemID, "server-a", "play-a", clientSession)
						}
						encoded, err := json.Marshal(body)
						if err != nil {
							t.Fatal(err)
						}
						requestCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
						defer cancel()
						req := httptest.NewRequest(http.MethodPost, path, bytes.NewReader(encoded)).WithContext(requestCtx)
						req.Header.Set("Content-Type", "application/json")
						req.Header.Set("X-Emby-Token", token)
						req.Header.Set("X-Emby-Device-Id", "xbox-001")
						rr := httptest.NewRecorder()
						done := make(chan struct{})
						go func() {
							defer close(done)
							handler.ServeHTTP(rr, req)
						}()
						defer func() {
							unblock()
							cancel()
							select {
							case <-done:
							case <-time.After(5 * time.Second):
								t.Error("session handler did not terminate during cleanup")
							}
						}()

						var observed phase7JObservedSession
						select {
						case observed = <-reached:
						case <-done:
							t.Fatalf("handler returned before reaching upstream: status=%d body=%s", rr.Code, rr.Body.String())
						case <-time.After(5 * time.Second):
							t.Fatal("session request did not reach upstream")
						}
						if observed.err != nil {
							t.Fatalf("decode forwarded session: %v", observed.err)
						}
						if observed.body["ItemId"] != "item-a" || observed.body["UserId"] != "user-a" || observed.body["PlaySessionId"] != "play-a" {
							t.Fatalf("incorrect translated session identity: %#v", observed.body)
						}
						if routeKind == "media" && observed.body["MediaSourceId"] != "ms-a" {
							t.Fatalf("incorrect translated MediaSourceId: %#v", observed.body)
						}
						phase7IAssertLocalStateUnchanged(t, app, info, itemID, beforeProgress, beforeLease)
						pendingProgress := app.WatchStore.GetProgress(info.UserID, itemID)
						if pendingProgress == nil || pendingProgress.Played != beforeProgress.Played || pendingProgress.IsFavorite != beforeProgress.IsFavorite {
							t.Fatalf("pending confirmation changed user state: %#v", pendingProgress)
						}
						select {
						case <-done:
							t.Fatal("handler completed while upstream response was blocked")
						default:
						}

						unblock()
						select {
						case <-done:
						case <-time.After(5 * time.Second):
							t.Fatal("handler did not complete after upstream confirmation")
						}
						if rr.Code != http.StatusNoContent || rr.Body.Len() != 0 || sessionHits.Load() != 1 {
							t.Fatalf("success status=%d body=%q upstream hits=%d", rr.Code, rr.Body.String(), sessionHits.Load())
						}
						afterProgress := app.WatchStore.GetProgress(info.UserID, itemID)
						if afterProgress == nil || afterProgress.PositionTicks != 900 || afterProgress.RuntimeTicks != 2000 ||
							afterProgress.LastPlayed <= beforeProgress.LastPlayed || afterProgress.UpdatedAt <= beforeProgress.UpdatedAt ||
							afterProgress.ServerID != "server-a" || afterProgress.OriginalItemID != "item-a" ||
							afterProgress.ProxyUserID != info.UserID || afterProgress.VirtualItemID != itemID ||
							afterProgress.Played != beforeProgress.Played || afterProgress.IsFavorite != beforeProgress.IsFavorite {
							t.Fatalf("confirmed success did not commit expected watch state: %#v", afterProgress)
						}
						afterLease := phase7ISnapshotLease(t, app, info.UserID, "server-a")
						if afterLease.DeviceID != beforeLease.DeviceID || afterLease.ItemID != beforeLease.ItemID ||
							afterLease.PlaySessionID != beforeLease.PlaySessionID || afterLease.Revision != beforeLease.Revision ||
							afterLease.LastHeartbeat <= beforeLease.LastHeartbeat || app.PlaybackLimiter.CountForServer("server-a") != 1 {
							t.Fatalf("confirmed success changed lease ownership or failed to heartbeat: before=%+v after=%+v", beforeLease, afterLease)
						}
					})
				})
			}
		}
	}
}
