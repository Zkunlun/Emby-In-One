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

func TestPhase6DTokenDeviceIdentityIsTokenScopedAndPersisted(t *testing.T) {
	withTempApp(t, func(app *App, _ http.Handler) {
		user, err := app.UserStore.Create("phase6d-user", "password123", nil)
		if err != nil {
			t.Fatalf("create user: %v", err)
		}
		hasPassword, err := app.UserStore.hasPassword(user)
		if err != nil {
			t.Fatalf("resolve password state: %v", err)
		}

		_, xboxToken, err := app.Auth.AuthenticateUser(user, hasPassword, " xbox-001 ")
		if err != nil {
			t.Fatalf("issue xbox token: %v", err)
		}
		_, phoneToken, err := app.Auth.AuthenticateUser(user, hasPassword, "phone-001")
		if err != nil {
			t.Fatalf("issue phone token: %v", err)
		}
		if xboxToken == phoneToken {
			t.Fatal("distinct logins unexpectedly received the same token")
		}
		if info := app.Auth.ValidateToken(xboxToken); info == nil || info.DeviceID != "xbox-001" {
			t.Fatalf("xbox token DeviceID = %#v, want xbox-001", info)
		}
		if info := app.Auth.ValidateToken(phoneToken); info == nil || info.DeviceID != "phone-001" {
			t.Fatalf("phone token DeviceID = %#v, want phone-001", info)
		}

		raw, err := os.ReadFile(filepath.Join(app.ConfigStore.Snapshot().DataDir, "tokens.json"))
		if err != nil {
			t.Fatalf("read tokens.json: %v", err)
		}
		persisted := map[string]json.RawMessage{}
		if err := json.Unmarshal(raw, &persisted); err != nil {
			t.Fatalf("decode tokens.json: %v", err)
		}
		var xboxInfo tokenInfo
		if err := json.Unmarshal(persisted[xboxToken], &xboxInfo); err != nil {
			t.Fatalf("decode persisted xbox token: %v", err)
		}
		if xboxInfo.DeviceID != "xbox-001" {
			t.Fatalf("persisted xbox DeviceID = %q, want xbox-001", xboxInfo.DeviceID)
		}
	})
}

func TestPhase6DRegularLoginCapturesIdentityAndBindsTokenDeviceID(t *testing.T) {
	withTempApp(t, func(app *App, handler http.Handler) {
		if _, err := app.UserStore.Create("phase6d-login", "password123", nil); err != nil {
			t.Fatalf("create user: %v", err)
		}
		req := httptest.NewRequest(http.MethodPost, "/Users/AuthenticateByName", bytes.NewBufferString("{\"Username\":\"phase6d-login\",\"Pw\":\"password123\"}"))
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("X-Emby-Authorization", "MediaBrowser Client=\"Hills\", Device=\"Xbox\", DeviceId=\"xbox-login\", Version=\"1.0\"")
		rr := httptest.NewRecorder()
		handler.ServeHTTP(rr, req)
		if rr.Code != http.StatusOK {
			t.Fatalf("login status = %d, body=%s", rr.Code, rr.Body.String())
		}
		var response map[string]any
		if err := json.Unmarshal(rr.Body.Bytes(), &response); err != nil {
			t.Fatalf("decode login response: %v", err)
		}
		token, _ := response["AccessToken"].(string)
		if token == "" {
			t.Fatalf("missing access token: %#v", response)
		}
		if info := app.Auth.ValidateToken(token); info == nil || info.DeviceID != "xbox-login" {
			t.Fatalf("regular token DeviceID = %#v, want xbox-login", info)
		}
		if got := app.Identity.GetCaptured(token).Get("X-Emby-Device-Id"); got != "xbox-login" {
			t.Fatalf("captured regular token DeviceID = %q, want xbox-login", got)
		}
	})
}

func TestPhase6DOldTokenJSONWithoutDeviceIDRemainsCompatible(t *testing.T) {
	var info tokenInfo
	if err := json.Unmarshal([]byte("{\"userId\":\"u1\",\"username\":\"old\",\"role\":\"user\",\"allowedServers\":[],\"createdAt\":1}"), &info); err != nil {
		t.Fatalf("unmarshal legacy token: %v", err)
	}
	if info.DeviceID != "" {
		t.Fatalf("legacy token DeviceID = %q, want empty", info.DeviceID)
	}
}
