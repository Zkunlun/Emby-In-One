package backend

import (
	"encoding/json"
	"net/http"
	"strings"
	"testing"
)

func TestEmbyUserPasswordFlagsAcrossUserAPIs(t *testing.T) {
	withTempApp(t, func(app *App, handler http.Handler) {
		emptyUser, err := app.UserStore.Create("flags-empty", "", nil)
		if err != nil {
			t.Fatalf("create empty-password user: %v", err)
		}
		normalUser, err := app.UserStore.Create("flags-normal", "password123", nil)
		if err != nil {
			t.Fatalf("create normal user: %v", err)
		}

		publicRR := doJSONRequest(t, handler, http.MethodGet, "/Users/Public", nil, "")
		if publicRR.Code != http.StatusOK {
			t.Fatalf("public users status = %d, body=%s", publicRR.Code, publicRR.Body.String())
		}
		var publicUsers []map[string]any
		if err := json.Unmarshal(publicRR.Body.Bytes(), &publicUsers); err != nil {
			t.Fatalf("decode public users: %v", err)
		}
		assertUserPasswordFlags2H(t, publicUserByName2H(t, publicUsers, "admin"), true)
		assertUserPasswordFlags2H(t, publicUserByName2H(t, publicUsers, emptyUser.Username), false)
		assertUserPasswordFlags2H(t, publicUserByName2H(t, publicUsers, normalUser.Username), true)

		emptyLogin := doJSONRequest(t, handler, http.MethodPost, "/Users/AuthenticateByName", map[string]any{
			"Username": emptyUser.Username,
			"Pw":       "",
		}, "")
		if emptyLogin.Code != http.StatusOK {
			t.Fatalf("empty-password login status = %d, body=%s", emptyLogin.Code, emptyLogin.Body.String())
		}
		emptyLoginPayload := decodeMap2H(t, emptyLogin.Body.Bytes())
		emptyLoginObject, _ := emptyLoginPayload["User"].(map[string]any)
		assertUserPasswordFlags2H(t, emptyLoginObject, false)
		assertLocalPasswordDisabled2H(t, emptyLoginObject)
		emptyToken, _ := emptyLoginPayload["AccessToken"].(string)
		if emptyToken == "" {
			t.Fatal("empty-password login response missing AccessToken")
		}

		emptyObjectRR := doJSONRequest(t, handler, http.MethodGet, "/Users/"+emptyUser.ID, nil, emptyToken)
		if emptyObjectRR.Code != http.StatusOK {
			t.Fatalf("empty user object status = %d, body=%s", emptyObjectRR.Code, emptyObjectRR.Body.String())
		}
		emptyObject := decodeMap2H(t, emptyObjectRR.Body.Bytes())
		assertUserPasswordFlags2H(t, emptyObject, false)
		assertLocalPasswordDisabled2H(t, emptyObject)

		normalLogin := doJSONRequest(t, handler, http.MethodPost, "/Users/AuthenticateByName", map[string]any{
			"Username": normalUser.Username,
			"Pw":       "password123",
		}, "")
		if normalLogin.Code != http.StatusOK {
			t.Fatalf("normal login status = %d, body=%s", normalLogin.Code, normalLogin.Body.String())
		}
		normalLoginPayload := decodeMap2H(t, normalLogin.Body.Bytes())
		normalLoginObject, _ := normalLoginPayload["User"].(map[string]any)
		assertUserPasswordFlags2H(t, normalLoginObject, true)
		assertLocalPasswordDisabled2H(t, normalLoginObject)

		adminLogin := doJSONRequest(t, handler, http.MethodPost, "/Users/AuthenticateByName", map[string]any{
			"Username": "admin",
			"Pw":       "secret",
		}, "")
		if adminLogin.Code != http.StatusOK {
			t.Fatalf("admin login status = %d, body=%s", adminLogin.Code, adminLogin.Body.String())
		}
		adminLoginPayload := decodeMap2H(t, adminLogin.Body.Bytes())
		adminObject, _ := adminLoginPayload["User"].(map[string]any)
		assertUserPasswordFlags2H(t, adminObject, true)
		assertLocalPasswordDisabled2H(t, adminObject)
	})
}

func TestUserPasswordFlagEndpointsFailClosedOnSecretDecryptError(t *testing.T) {
	withTempApp(t, func(app *App, handler http.Handler) {
		user, err := app.UserStore.Create("flags-corrupt", "password123", nil)
		if err != nil {
			t.Fatalf("create user: %v", err)
		}
		hasPassword, err := app.UserStore.hasPassword(user)
		if err != nil {
			t.Fatalf("resolve initial password state: %v", err)
		}
		_, existingToken, err := app.Auth.AuthenticateUser(user, hasPassword)
		if err != nil {
			t.Fatalf("issue initial token: %v", err)
		}

		app.UserStore.mu.Lock()
		app.UserStore.users[user.ID].PasswordSecret = "v1:not-valid-base64"
		app.UserStore.mu.Unlock()

		for _, tc := range []struct {
			name   string
			method string
			path   string
			body   any
			token  string
		}{
			{name: "public users", method: http.MethodGet, path: "/Users/Public"},
			{name: "authenticated user object", method: http.MethodGet, path: "/Users/" + user.ID, token: existingToken},
			{name: "authenticate response", method: http.MethodPost, path: "/Users/AuthenticateByName", body: map[string]any{"Username": user.Username, "Pw": "password123"}},
		} {
			t.Run(tc.name, func(t *testing.T) {
				rr := doJSONRequest(t, handler, tc.method, tc.path, tc.body, tc.token)
				if rr.Code != http.StatusInternalServerError {
					t.Fatalf("status = %d, want 500; body=%s", rr.Code, rr.Body.String())
				}
				if strings.Contains(rr.Body.String(), "not-valid-base64") {
					t.Fatalf("decrypt failure leaked password secret material: %s", rr.Body.String())
				}
			})
		}

		app.Auth.mu.RLock()
		issuedTokens := len(app.Auth.tokens)
		app.Auth.mu.RUnlock()
		if issuedTokens != 1 || !app.Auth.HasIssuedToken(existingToken) {
			t.Fatalf("decrypt failure issued or replaced a token: token count=%d existingPresent=%v", issuedTokens, app.Auth.HasIssuedToken(existingToken))
		}
	})
}

func assertUserPasswordFlags2H(t *testing.T, user map[string]any, hasPassword bool) {
	t.Helper()
	if user == nil {
		t.Fatal("user object is nil")
	}
	if got, ok := user["HasPassword"].(bool); !ok || got != hasPassword {
		t.Fatalf("HasPassword = %#v, want %v; user=%#v", user["HasPassword"], hasPassword, user)
	}
	if got, ok := user["HasConfiguredPassword"].(bool); !ok || got != hasPassword {
		t.Fatalf("HasConfiguredPassword = %#v, want %v; user=%#v", user["HasConfiguredPassword"], hasPassword, user)
	}
	if got, ok := user["HasConfiguredEasyPassword"].(bool); !ok || got {
		t.Fatalf("HasConfiguredEasyPassword = %#v, want false; user=%#v", user["HasConfiguredEasyPassword"], user)
	}
	if got, ok := user["EnableAutoLogin"].(bool); !ok || got {
		t.Fatalf("EnableAutoLogin = %#v, want false; user=%#v", user["EnableAutoLogin"], user)
	}
}

func assertLocalPasswordDisabled2H(t *testing.T, user map[string]any) {
	t.Helper()
	configuration, _ := user["Configuration"].(map[string]any)
	if configuration == nil {
		t.Fatalf("user object missing Configuration: %#v", user)
	}
	if got, ok := configuration["EnableLocalPassword"].(bool); !ok || got {
		t.Fatalf("Configuration.EnableLocalPassword = %#v, want false", configuration["EnableLocalPassword"])
	}
}

func publicUserByName2H(t *testing.T, users []map[string]any, name string) map[string]any {
	t.Helper()
	for _, user := range users {
		if got, _ := user["Name"].(string); got == name {
			return user
		}
	}
	t.Fatalf("public user %q not found in %#v", name, users)
	return nil
}

func decodeMap2H(t *testing.T, raw []byte) map[string]any {
	t.Helper()
	var payload map[string]any
	if err := json.Unmarshal(raw, &payload); err != nil {
		t.Fatalf("decode JSON object: %v", err)
	}
	return payload
}
