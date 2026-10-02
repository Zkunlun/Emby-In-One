package backend

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
)

func phase5EFailingPlaybackUpstream(t *testing.T) *httptest.Server {
	t.Helper()
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodPost && r.URL.Path == "/Users/AuthenticateByName":
			_ = json.NewEncoder(w).Encode(map[string]any{
				"AccessToken": "tok-a",
				"User":        map[string]any{"Id": "user-a"},
			})
		default:
			w.WriteHeader(http.StatusServiceUnavailable)
		}
	}))
}

func TestPhase5EPlaybackInfoFailureRollsBackOnlyCreatedReservation(t *testing.T) {
	t.Run("first failed playback leaves no lease", func(t *testing.T) {
		upstream := phase5EFailingPlaybackUpstream(t)
		defer upstream.Close()

		withTempAppConfig(t, playbackLimiterConfig(upstream.URL), func(app *App, handler http.Handler) {
			token := createRegularUser(t, handler)
			serverID := app.Upstream.Clients()[0].ID
			item := app.IDStore.GetOrCreateVirtualID("item-a", serverID)

			rr := doAuthJSON(t, handler, http.MethodGet, "/Items/"+item+"/PlaybackInfo", nil, token)
			if rr.Code != http.StatusBadGateway {
				t.Fatalf("PlaybackInfo status=%d body=%s, want 502", rr.Code, rr.Body.String())
			}
			if got := app.PlaybackLimiter.CountForServer(serverID); got != 0 {
				t.Fatalf("created provisional lease leaked after total failure: count=%d", got)
			}
		})
	})

	t.Run("failed same-device retry preserves committed lease", func(t *testing.T) {
		upstream := phase5EFailingPlaybackUpstream(t)
		defer upstream.Close()

		withTempAppConfig(t, playbackLimiterConfig(upstream.URL), func(app *App, handler http.Handler) {
			token := createRegularUser(t, handler)
			info := app.Auth.ValidateToken(token)
			if info == nil {
				t.Fatal("regular token missing")
			}
			serverID := app.Upstream.Clients()[0].ID
			itemA := app.IDStore.GetOrCreateVirtualID("item-a", serverID)
			itemB := app.IDStore.GetOrCreateVirtualID("item-b", serverID)

			if info.DeviceID == "" {
				t.Fatal("same-device retry fixture requires the token DeviceID")
			}
			committed := app.PlaybackLimiter.Reserve(info.UserID, serverID, info.DeviceID, itemA, "session-a")
			if !committed.Allowed || !committed.Created {
				t.Fatalf("seed committed lease = %+v, want allowed+created", committed)
			}

			rr := doAuthJSON(t, handler, http.MethodGet, "/Items/"+itemB+"/PlaybackInfo", nil, token)
			if rr.Code != http.StatusBadGateway {
				t.Fatalf("PlaybackInfo retry status=%d body=%s, want 502", rr.Code, rr.Body.String())
			}

			key := streamKey{UserID: info.UserID, ServerID: serverID}
			app.PlaybackLimiter.mu.Lock()
			entry := app.PlaybackLimiter.streams[key]
			app.PlaybackLimiter.mu.Unlock()
			if entry == nil {
				t.Fatal("failed same-device retry deleted existing committed lease")
			}
			if entry.ItemID != itemA || entry.PlaySessionID != "session-a" {
				t.Fatalf("existing lease state changed after failed retry: %+v", entry)
			}
			if entry.Revision == committed.Revision {
				t.Fatalf("retry did not advance reservation revision: entry=%+v seed=%+v", entry, committed)
			}
		})
	})
}
