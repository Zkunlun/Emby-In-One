package backend

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
)

func phase6LLoginToken(t *testing.T, handler http.Handler, username, password, deviceID string) string {
	t.Helper()
	payload, err := json.Marshal(map[string]any{"Username": username, "Pw": password})
	if err != nil {
		t.Fatalf("marshal login payload: %v", err)
	}
	req := httptest.NewRequest(http.MethodPost, "/Users/AuthenticateByName", bytes.NewReader(payload))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-Emby-Authorization", `MediaBrowser Client="Phase6L", Device="Test Device", DeviceId="`+deviceID+`", Version="1.0"`)
	rr := httptest.NewRecorder()
	handler.ServeHTTP(rr, req)
	if rr.Code != http.StatusOK {
		t.Fatalf("login %s/%s status=%d body=%s", username, deviceID, rr.Code, rr.Body.String())
	}
	var response map[string]any
	if err := json.Unmarshal(rr.Body.Bytes(), &response); err != nil {
		t.Fatalf("decode login response: %v", err)
	}
	token, _ := response["AccessToken"].(string)
	if token == "" {
		t.Fatalf("login %s/%s returned no token: %#v", username, deviceID, response)
	}
	return token
}

func phase6LPersistedTokens(t *testing.T, app *App) map[string]json.RawMessage {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join(app.ConfigStore.Snapshot().DataDir, "tokens.json"))
	if err != nil {
		t.Fatalf("read tokens.json: %v", err)
	}
	persisted := map[string]json.RawMessage{}
	if err := json.Unmarshal(raw, &persisted); err != nil {
		t.Fatalf("decode tokens.json: %v", err)
	}
	return persisted
}

func TestPhase6LTokenDeviceIdentityIsolationAndSingleRevoke(t *testing.T) {
	withTempApp(t, func(app *App, handler http.Handler) {
		user, err := app.UserStore.Create("phase6l-user", "password123", nil)
		if err != nil {
			t.Fatalf("create user: %v", err)
		}

		xboxToken := phase6LLoginToken(t, handler, user.Username, "password123", "xbox-001")
		phoneToken := phase6LLoginToken(t, handler, user.Username, "password123", "phone-001")
		if xboxToken == phoneToken {
			t.Fatal("distinct device logins unexpectedly received the same token")
		}

		if info := app.Auth.ValidateToken(xboxToken); info == nil || info.UserID != user.ID || info.DeviceID != "xbox-001" {
			t.Fatalf("xbox token identity = %#v", info)
		}
		if info := app.Auth.ValidateToken(phoneToken); info == nil || info.UserID != user.ID || info.DeviceID != "phone-001" {
			t.Fatalf("phone token identity = %#v", info)
		}
		if got := app.Identity.GetCaptured(xboxToken).Get("X-Emby-Device-Id"); got != "xbox-001" {
			t.Fatalf("xbox captured DeviceID=%q, want xbox-001", got)
		}
		if got := app.Identity.GetCaptured(phoneToken).Get("X-Emby-Device-Id"); got != "phone-001" {
			t.Fatalf("phone captured DeviceID=%q, want phone-001", got)
		}

		if !app.Auth.RevokeToken(xboxToken) {
			t.Fatal("xbox token revoke returned false")
		}
		if app.Auth.ValidateToken(xboxToken) != nil {
			t.Fatal("revoked xbox token still validates")
		}
		if got := app.Identity.GetCaptured(xboxToken).Get("X-Emby-Device-Id"); got != "" {
			t.Fatalf("revoked xbox captured identity survived: %q", got)
		}

		if info := app.Auth.ValidateToken(phoneToken); info == nil || info.DeviceID != "phone-001" {
			t.Fatalf("revoking xbox token mutated phone token: %#v", info)
		}
		if got := app.Identity.GetCaptured(phoneToken).Get("X-Emby-Device-Id"); got != "phone-001" {
			t.Fatalf("revoking xbox token mutated phone capture: %q", got)
		}
		persisted := phase6LPersistedTokens(t, app)
		if _, ok := persisted[xboxToken]; ok {
			t.Fatal("revoked xbox token remained in tokens.json")
		}
		if _, ok := persisted[phoneToken]; !ok {
			t.Fatal("unrelated phone token disappeared from tokens.json")
		}
	})
}

func TestPhase6LRevokeTokensByUserIDClearsAllDeviceIdentityForThatUserOnly(t *testing.T) {
	withTempApp(t, func(app *App, handler http.Handler) {
		userA, err := app.UserStore.Create("phase6l-a", "password123", nil)
		if err != nil {
			t.Fatalf("create user A: %v", err)
		}
		userB, err := app.UserStore.Create("phase6l-b", "password123", nil)
		if err != nil {
			t.Fatalf("create user B: %v", err)
		}

		aXbox := phase6LLoginToken(t, handler, userA.Username, "password123", "a-xbox")
		aPhone := phase6LLoginToken(t, handler, userA.Username, "password123", "a-phone")
		bPhone := phase6LLoginToken(t, handler, userB.Username, "password123", "b-phone")

		app.Auth.RevokeTokensByUserID(userA.ID)
		for _, token := range []string{aXbox, aPhone} {
			if app.Auth.ValidateToken(token) != nil {
				t.Fatalf("revoked user-A token still validates: %s", token)
			}
			if got := app.Identity.GetCaptured(token).Get("X-Emby-Device-Id"); got != "" {
				t.Fatalf("revoked user-A captured identity survived: %q", got)
			}
		}
		if info := app.Auth.ValidateToken(bPhone); info == nil || info.UserID != userB.ID || info.DeviceID != "b-phone" {
			t.Fatalf("user-B token was affected by user-A revoke: %#v", info)
		}
		if got := app.Identity.GetCaptured(bPhone).Get("X-Emby-Device-Id"); got != "b-phone" {
			t.Fatalf("user-B capture was affected by user-A revoke: %q", got)
		}

		persisted := phase6LPersistedTokens(t, app)
		if _, ok := persisted[aXbox]; ok {
			t.Fatal("revoked user-A xbox token remained persisted")
		}
		if _, ok := persisted[aPhone]; ok {
			t.Fatal("revoked user-A phone token remained persisted")
		}
		if _, ok := persisted[bPhone]; !ok {
			t.Fatal("user-B token disappeared from persistence")
		}
	})
}

func TestPhase6LRevokeAllTokensClearsPersistedAndCapturedDeviceIdentity(t *testing.T) {
	withTempApp(t, func(app *App, handler http.Handler) {
		user, err := app.UserStore.Create("phase6l-all", "password123", nil)
		if err != nil {
			t.Fatalf("create user: %v", err)
		}
		userToken := phase6LLoginToken(t, handler, user.Username, "password123", "user-device")
		adminToken := phase6LLoginToken(t, handler, "admin", "secret", "admin-device")

		proxyUserID := app.Auth.ProxyUserID()
		if proxyUserID == "" {
			t.Fatal("proxy identity missing before token revocation")
		}
		app.Auth.RevokeAllTokens()
		for _, token := range []string{userToken, adminToken} {
			if app.Auth.ValidateToken(token) != nil {
				t.Fatalf("token still validates after RevokeAllTokens: %s", token)
			}
			if got := app.Identity.GetCaptured(token).Get("X-Emby-Device-Id"); got != "" {
				t.Fatalf("captured identity survived RevokeAllTokens: %q", got)
			}
		}
		persisted := phase6LPersistedTokens(t, app)
		if len(persisted) != 1 {
			t.Fatalf("tokens.json must retain only proxy identity after RevokeAllTokens: %#v", persisted)
		}
		var storedProxyUserID string
		if err := json.Unmarshal(persisted["_proxyUserId"], &storedProxyUserID); err != nil {
			t.Fatalf("decode persisted proxy identity: %v", err)
		}
		if storedProxyUserID != proxyUserID || app.Auth.ProxyUserID() != proxyUserID {
			t.Fatalf("token revocation changed proxy identity: stored=%q current=%q want=%q", storedProxyUserID, app.Auth.ProxyUserID(), proxyUserID)
		}
	})
}
