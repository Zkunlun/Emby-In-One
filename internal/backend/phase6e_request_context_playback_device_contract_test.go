package backend

import (
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestPhase6EWithContextInjectsResolvedPlaybackDeviceID(t *testing.T) {
	withTempApp(t, func(app *App, _ http.Handler) {
		user, err := app.UserStore.Create("phase6e-user", "password123", nil)
		if err != nil {
			t.Fatalf("create user: %v", err)
		}
		hasPassword, err := app.UserStore.hasPassword(user)
		if err != nil {
			t.Fatalf("resolve password state: %v", err)
		}
		_, token, err := app.Auth.AuthenticateUser(user, hasPassword, "token-device")
		if err != nil {
			t.Fatalf("issue token: %v", err)
		}

		capture := func(req *http.Request) *RequestContext {
			t.Helper()
			var got *RequestContext
			handler := app.withContext(func(_ http.ResponseWriter, r *http.Request) {
				got = requestContextFrom(r.Context())
			})
			handler(httptest.NewRecorder(), req)
			if got == nil {
				t.Fatal("withContext did not inject RequestContext")
			}
			return got
		}

		req := httptest.NewRequest(http.MethodGet, "/test", nil)
		req.Header.Set("X-Emby-Token", token)
		if got := capture(req).PlaybackDeviceID; got != "token-device" {
			t.Fatalf("token fallback PlaybackDeviceID = %q, want token-device", got)
		}

		req = httptest.NewRequest(http.MethodGet, "/test", nil)
		req.Header.Set("X-Emby-Token", token)
		req.Header.Set("Authorization", "Emby DeviceId=\"auth-device\"")
		if got := capture(req).PlaybackDeviceID; got != "auth-device" {
			t.Fatalf("Authorization PlaybackDeviceID = %q, want auth-device", got)
		}

		req = httptest.NewRequest(http.MethodGet, "/test", nil)
		req.Header.Set("X-Emby-Token", token)
		req.Header.Set("Authorization", "Emby DeviceId=\"auth-device\"")
		req.Header.Set("X-Emby-Authorization", "MediaBrowser DeviceId=\"x-auth-device\"")
		if got := capture(req).PlaybackDeviceID; got != "x-auth-device" {
			t.Fatalf("X-Emby-Authorization PlaybackDeviceID = %q, want x-auth-device", got)
		}

		req = httptest.NewRequest(http.MethodGet, "/test", nil)
		req.Header.Set("X-Emby-Token", token)
		req.Header.Set("X-Emby-Authorization", "MediaBrowser DeviceId=\"x-auth-device\"")
		req.Header.Set("X-Emby-Device-Id", "direct-device")
		if got := capture(req).PlaybackDeviceID; got != "direct-device" {
			t.Fatalf("direct PlaybackDeviceID = %q, want direct-device", got)
		}
	})
}

func TestPhase6EUnknownTokenCannotSupplyPlaybackDeviceFallback(t *testing.T) {
	withTempApp(t, func(app *App, _ http.Handler) {
		var got *RequestContext
		handler := app.withContext(func(_ http.ResponseWriter, r *http.Request) {
			got = requestContextFrom(r.Context())
		})
		req := httptest.NewRequest(http.MethodGet, "/test", nil)
		req.Header.Set("X-Emby-Token", "unknown-token")
		handler(httptest.NewRecorder(), req)
		if got == nil {
			t.Fatal("withContext did not inject RequestContext")
		}
		if got.ProxyUser != nil {
			t.Fatalf("unknown token unexpectedly validated: %#v", got.ProxyUser)
		}
		if got.PlaybackDeviceID != "" {
			t.Fatalf("unknown token PlaybackDeviceID = %q, want empty", got.PlaybackDeviceID)
		}
	})
}
