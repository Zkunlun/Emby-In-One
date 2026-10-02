package backend

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"
)

// TestPhase6KFailClosedLifecycleMatrix consolidates Phase 6's identity boundary.
// Detailed endpoint fixtures remain in Phase 1E/1G and Phase 6F-6H; this matrix
// keeps the cross-cutting contract visible in one place.
func TestPhase6KFailClosedLifecycleMatrix(t *testing.T) {
	t.Run("01 regular PlaybackInfo without any DeviceID source fails before upstream and lease", func(t *testing.T) {
		var playbackInfoHits atomic.Int32
		upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			switch {
			case r.Method == http.MethodPost && r.URL.Path == "/Users/AuthenticateByName":
				_ = json.NewEncoder(w).Encode(map[string]any{"AccessToken": "tok-a", "User": map[string]any{"Id": "user-a"}})
			case r.URL.Path == "/Items/item-a/PlaybackInfo":
				playbackInfoHits.Add(1)
				_ = json.NewEncoder(w).Encode(map[string]any{
					"MediaSources":  []map[string]any{{"Id": "ms-a", "ItemId": "item-a", "Container": "mp4"}},
					"PlaySessionId": "play-a",
				})
			default:
				w.WriteHeader(http.StatusOK)
			}
		}))
		defer upstream.Close()

		withTempAppConfig(t, phase1DPlaybackConfig(upstream.URL, 1), func(app *App, handler http.Handler) {
			token := phase1ECreateAuthorizedUser(t, handler, []string{"server-a"})
			phase6FClearTokenDeviceIdentity(app, token)
			itemID := app.IDStore.GetOrCreateVirtualID("item-a", "server-a")
			req := httptest.NewRequest(http.MethodGet, "/Items/"+itemID+"/PlaybackInfo", nil)
			req.Header.Set("X-Emby-Token", token)
			rr := httptest.NewRecorder()
			handler.ServeHTTP(rr, req)

			if rr.Code != http.StatusBadRequest || phase1GErrorCode(t, rr) != playbackDeviceIDRequiredCode {
				t.Fatalf("missing DeviceID PlaybackInfo: status=%d body=%s", rr.Code, rr.Body.String())
			}
			if playbackInfoHits.Load() != 0 || app.PlaybackLimiter.CountForServer("server-a") != 0 {
				t.Fatalf("missing DeviceID crossed fail-closed boundary: upstreamHits=%d leases=%d", playbackInfoHits.Load(), app.PlaybackLimiter.CountForServer("server-a"))
			}
		})
	})

	t.Run("02 admin PlaybackInfo without DeviceID stays outside limiter", func(t *testing.T) {
		upstream := phase1ELifecycleUpstream(t, "tok-a", "user-a")
		defer upstream.Close()
		withTempAppConfig(t, phase1DPlaybackConfig(upstream.URL, 1), func(app *App, handler http.Handler) {
			token := loginTokenAs(t, handler, "admin", "secret")
			phase6FClearTokenDeviceIdentity(app, token)
			itemID := app.IDStore.GetOrCreateVirtualID("item-a", "server-a")
			req := httptest.NewRequest(http.MethodGet, "/Items/"+itemID+"/PlaybackInfo", nil)
			req.Header.Set("X-Emby-Token", token)
			rr := httptest.NewRecorder()
			handler.ServeHTTP(rr, req)
			if rr.Code != http.StatusOK {
				t.Fatalf("admin PlaybackInfo without DeviceID: status=%d body=%s", rr.Code, rr.Body.String())
			}
			if got := app.PlaybackLimiter.CountForServer("server-a"); got != 0 {
				t.Fatalf("admin created playback lease: count=%d", got)
			}
		})
	})

	t.Run("03 second device is denied while owner remains accepted", func(t *testing.T) {
		l := NewPlaybackLimiter()
		first := l.Reserve("user", "server-a", "xbox", "item-a", "session-a")
		if !first.Allowed {
			t.Fatal("owner device reserve rejected")
		}
		if second := l.Reserve("user", "server-a", "phone", "item-a", "session-a"); second.Allowed {
			t.Fatalf("second device unexpectedly allowed: %+v", second)
		}
		if again := l.Reserve("user", "server-a", "xbox", "item-b", "session-b"); !again.Allowed {
			t.Fatalf("owner device lost lease after second-device rejection: %+v", again)
		}
	})

	t.Run("04 public conflict errors stay distinct", func(t *testing.T) {
		missing := httptest.NewRecorder()
		writePlaybackDeviceIDRequired(missing)
		if missing.Code != http.StatusBadRequest || phase1GErrorCode(t, missing) != playbackDeviceIDRequiredCode {
			t.Fatalf("missing-device public error: status=%d body=%s", missing.Code, missing.Body.String())
		}
		conflict := httptest.NewRecorder()
		writePlaybackDeviceLimit(conflict)
		if conflict.Code != http.StatusTooManyRequests || phase1GErrorCode(t, conflict) != playbackDeviceLimitCode {
			t.Fatalf("second-device public error: status=%d body=%s", conflict.Code, conflict.Body.String())
		}
	})

	t.Run("05 owner Playing and Progress heartbeat while wrong or missing identity cannot", func(t *testing.T) {
		upstream := phase1DPlaybackUpstream(t)
		defer upstream.Close()
		withTempAppConfig(t, phase1DPlaybackConfig(upstream.URL, 1), func(app *App, _ http.Handler) {
			if result := app.PlaybackLimiter.Reserve("user-a", "server-a", "xbox", "item-a", "session-a"); !result.Allowed {
				t.Fatal("seed lease rejected")
			}
			old := time.Now().Add(-time.Minute).Round(0)
			phase5KAgeLease(app.PlaybackLimiter, "user-a", "server-a", old)
			owner := &RequestContext{ProxyUser: &tokenInfo{UserID: "user-a", Role: "user", AllowedServers: []string{"server-a"}}, PlaybackDeviceID: "xbox"}
			wrong := &RequestContext{ProxyUser: &tokenInfo{UserID: "user-a", Role: "user", AllowedServers: []string{"server-a"}}, PlaybackDeviceID: "phone"}
			missing := &RequestContext{ProxyUser: &tokenInfo{UserID: "user-a", Role: "user", AllowedServers: []string{"server-a"}}}

			if !app.heartbeatPlaybackLease(owner, "server-a") {
				t.Fatal("owner Playing heartbeat rejected")
			}
			first := phase5KLeaseSnapshot(app.PlaybackLimiter, "user-a", "server-a")
			if first == nil || !first.LastHeartbeat.After(old) {
				t.Fatalf("owner heartbeat did not refresh lease: %+v", first)
			}
			stable := first.LastHeartbeat
			if app.heartbeatPlaybackLease(wrong, "server-a") {
				t.Fatal("wrong-device Progress heartbeat accepted")
			}
			if app.heartbeatPlaybackLease(missing, "server-a") {
				t.Fatal("missing-device Progress heartbeat accepted")
			}
			after := phase5KLeaseSnapshot(app.PlaybackLimiter, "user-a", "server-a")
			if after == nil || !after.LastHeartbeat.Equal(stable) {
				t.Fatalf("wrong/missing identity changed heartbeat: before=%v after=%+v", stable, after)
			}
		})
	})

	t.Run("06 Stopped requires exact owner device and PlaySessionID", func(t *testing.T) {
		upstream := phase1DPlaybackUpstream(t)
		defer upstream.Close()
		withTempAppConfig(t, phase1DPlaybackConfig(upstream.URL, 1), func(app *App, _ http.Handler) {
			owner := &RequestContext{ProxyUser: &tokenInfo{UserID: "user-a", Role: "user", AllowedServers: []string{"server-a"}}, PlaybackDeviceID: "xbox"}
			wrong := &RequestContext{ProxyUser: &tokenInfo{UserID: "user-a", Role: "user", AllowedServers: []string{"server-a"}}, PlaybackDeviceID: "phone"}
			missing := &RequestContext{ProxyUser: &tokenInfo{UserID: "user-a", Role: "user", AllowedServers: []string{"server-a"}}}
			app.PlaybackLimiter.Reserve("user-a", "server-a", "xbox", "item-a", "session-a")

			if app.stopPlaybackLease(wrong, "server-a", "session-a") {
				t.Fatal("wrong device released lease")
			}
			if app.stopPlaybackLease(missing, "server-a", "session-a") {
				t.Fatal("missing device released lease")
			}
			if app.stopPlaybackLease(owner, "server-a", "session-old") {
				t.Fatal("old PlaySessionID released current lease")
			}
			if !app.stopPlaybackLease(owner, "server-a", "session-a") {
				t.Fatal("exact owner/session did not release lease")
			}
			if got := app.PlaybackLimiter.CountForServer("server-a"); got != 0 {
				t.Fatalf("matching Stopped left lease behind: count=%d", got)
			}
		})
	})

	t.Run("07 missing identity stream switch preserves source lease and route", func(t *testing.T) {
		upstreamA := phase1ELifecycleUpstream(t, "tok-a", "user-a")
		defer upstreamA.Close()
		upstreamB := phase1ELifecycleUpstream(t, "tok-b", "user-b")
		defer upstreamB.Close()
		withTempAppConfig(t, phase1EDualPlaybackConfig(upstreamA.URL, upstreamB.URL), func(app *App, handler http.Handler) {
			token := phase1ECreateAuthorizedUser(t, handler, []string{"server-a", "server-b"})
			info := app.Auth.ValidateToken(token)
			if info == nil {
				t.Fatal("regular token missing")
			}
			item := app.IDStore.GetOrCreateVirtualID("item-a", "server-a")
			app.IDStore.AssociateAdditionalInstance(item, "item-b", "server-b")
			app.PlaybackLimiter.Reserve(info.UserID, "server-a", "xbox", item, "play-a")
			reqCtx := &RequestContext{ProxyToken: token, ProxyUser: info}
			owner := playbackRouteOwner(reqCtx)
			app.playbackRoutes.Activate(owner, item, "server-a", "play-a", "client-play-a")
			app.IDStore.SetActiveStream(item, "server-a")
			req := httptest.NewRequest(http.MethodGet, "/Videos/"+item+"/stream.mp4", nil)
			req = req.WithContext(context.WithValue(req.Context(), requestContextKey{}, reqCtx))
			rr := httptest.NewRecorder()

			if app.switchPlaybackLease(rr, req, "server-b", "server-a", item, "play-b", "play-a", "client-play-a") {
				t.Fatal("missing-device stream switch unexpectedly succeeded")
			}
			if rr.Code != http.StatusBadRequest || phase1GErrorCode(t, rr) != playbackDeviceIDRequiredCode {
				t.Fatalf("missing-device stream switch: status=%d body=%s", rr.Code, rr.Body.String())
			}
			if app.PlaybackLimiter.CountForServer("server-a") != 1 || app.PlaybackLimiter.CountForServer("server-b") != 0 {
				t.Fatalf("stream switch mutated leases: source=%d target=%d", app.PlaybackLimiter.CountForServer("server-a"), app.PlaybackLimiter.CountForServer("server-b"))
			}
			active, ok := app.playbackRoutes.Active(owner, item)
			if !ok || active.ServerID != "server-a" || active.PlaySessionID != "play-a" {
				t.Fatalf("stream switch mutated active route: %+v ok=%v", active, ok)
			}
		})
	})
}
