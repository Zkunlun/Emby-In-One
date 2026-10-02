package backend

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
)

func phase1GErrorCode(t *testing.T, rr *httptest.ResponseRecorder) string {
	t.Helper()
	var payload map[string]any
	if err := json.Unmarshal(rr.Body.Bytes(), &payload); err != nil {
		t.Fatalf("decode error response: %v body=%s", err, rr.Body.String())
	}
	code, _ := payload["code"].(string)
	if code == "" {
		t.Fatalf("error response missing code: status=%d body=%s", rr.Code, rr.Body.String())
	}
	return code
}

func TestPhase1GAuthorizationCapacityFullErrorContract(t *testing.T) {
	upstream := phase1AUpstreamStub(t)
	defer upstream.Close()

	withTempAppConfig(t, phase1AAuthorizationSlotConfig(upstream.URL), func(app *App, handler http.Handler) {
		adminToken := loginTokenAs(t, handler, "admin", "secret")
		for _, username := range []string{"alice", "bob"} {
			if rr := phase1ACreateUser(t, handler, adminToken, username, []string{"server-a"}); rr.Code != http.StatusCreated {
				t.Fatalf("seed user %s: status=%d body=%s", username, rr.Code, rr.Body.String())
			}
		}

		rr := phase1ACreateUser(t, handler, adminToken, "charlie", []string{"server-a"})
		if rr.Code != http.StatusConflict {
			t.Fatalf("full authorization capacity status=%d, want 409 body=%s", rr.Code, rr.Body.String())
		}
		if code := phase1GErrorCode(t, rr); code != "UPSTREAM_CAPACITY_FULL" {
			t.Fatalf("full authorization capacity code=%q, want UPSTREAM_CAPACITY_FULL", code)
		}
	})
}

func TestPhase1GMaxConcurrentBelowAssignedErrorContract(t *testing.T) {
	upstream := phase1AUpstreamStub(t)
	defer upstream.Close()

	config := replaceMaxConcurrentForPhase1C(t, phase1AAuthorizationSlotConfig(upstream.URL), 3)
	withTempAppConfig(t, config, func(app *App, handler http.Handler) {
		adminToken := loginTokenAs(t, handler, "admin", "secret")
		phase1CCreateAssignedUsers(t, handler, adminToken, 3)

		rr := doAuthJSON(t, handler, http.MethodPut, "/admin/api/upstream/server-a", map[string]any{
			"maxConcurrent": 2,
		}, adminToken)
		if rr.Code != http.StatusConflict {
			t.Fatalf("maxConcurrent below assigned status=%d, want 409 body=%s", rr.Code, rr.Body.String())
		}
		if code := phase1GErrorCode(t, rr); code != "UPSTREAM_CAPACITY_BELOW_ASSIGNED" {
			t.Fatalf("maxConcurrent conflict code=%q, want UPSTREAM_CAPACITY_BELOW_ASSIGNED", code)
		}
	})
}

func TestPhase1GSecondPlaybackDeviceErrorContract(t *testing.T) {
	upstream := phase1ELifecycleUpstream(t, "tok-a", "user-a")
	defer upstream.Close()

	withTempAppConfig(t, phase1DPlaybackConfig(upstream.URL, 1), func(app *App, handler http.Handler) {
		token := phase1ECreateAuthorizedUser(t, handler, []string{"server-a"})
		itemA := app.IDStore.GetOrCreateVirtualID("item-a", "server-a")

		if rr := phase1DPlaybackInfo(t, handler, token, itemA, "xbox-001"); rr.Code != http.StatusOK {
			t.Fatalf("Xbox PlaybackInfo: status=%d body=%s", rr.Code, rr.Body.String())
		}
		phone := phase1DPlaybackInfo(t, handler, token, itemA, "phone-001")
		if phone.Code != http.StatusTooManyRequests {
			t.Fatalf("second device status=%d, want 429 body=%s", phone.Code, phone.Body.String())
		}
		if code := phase1GErrorCode(t, phone); code != "PLAYBACK_DEVICE_LIMIT" {
			t.Fatalf("second device code=%q, want PLAYBACK_DEVICE_LIMIT", code)
		}
	})
}

func TestPhase1GMissingPlaybackDeviceIDErrorContract(t *testing.T) {
	upstream := phase1ELifecycleUpstream(t, "tok-a", "user-a")
	defer upstream.Close()

	withTempAppConfig(t, phase1DPlaybackConfig(upstream.URL, 1), func(app *App, handler http.Handler) {
		token := phase1ECreateAuthorizedUser(t, handler, []string{"server-a"})
		itemA := app.IDStore.GetOrCreateVirtualID("item-a", "server-a")

		// This contract means all four DeviceID sources are absent. The shared login
		// helper now binds device-1 to the token as Phase 6's fourth-priority fallback,
		// so remove that test fixture identity before issuing the header-less request.
		app.Auth.mu.Lock()
		info := app.Auth.tokens[token]
		info.DeviceID = ""
		app.Auth.tokens[token] = info
		app.Auth.mu.Unlock()
		app.Identity.DeleteCaptured(token)

		req := httptest.NewRequest(http.MethodGet, "/Items/"+itemA+"/PlaybackInfo", nil)
		req.Header.Set("X-Emby-Token", token)
		rr := httptest.NewRecorder()
		handler.ServeHTTP(rr, req)

		if rr.Code != http.StatusBadRequest {
			t.Fatalf("missing DeviceId status=%d, want 400 body=%s", rr.Code, rr.Body.String())
		}
		if code := phase1GErrorCode(t, rr); code != "PLAYBACK_DEVICE_ID_REQUIRED" {
			t.Fatalf("missing DeviceId code=%q, want PLAYBACK_DEVICE_ID_REQUIRED", code)
		}
	})
}
