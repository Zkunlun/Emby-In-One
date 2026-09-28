package backend

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

const (
	securityUpstreamPassword = "UPSTREAM-PASSWORD-SECRET"
	securityUpstreamAPIKey   = "UPSTREAM-APIKEY-SECRET"
	securityUserPassword     = "REGULAR-USER-PASSWORD-SECRET"
)

func securityCredentialConfig(upstreamURL string) string {
	return parityConfigWithUpstreams(fmt.Sprintf(`  - name: "Password upstream"
    url: %q
    username: "up-user"
    password: %q
  - name: "API key upstream"
    url: %q
    apiKey: %q
`, upstreamURL, securityUpstreamPassword, upstreamURL, securityUpstreamAPIKey))
}

func newSecurityCredentialUpstream(t *testing.T) *httptest.Server {
	t.Helper()
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodPost && r.URL.Path == "/Users/AuthenticateByName":
			_ = json.NewEncoder(w).Encode(map[string]any{
				"AccessToken": "password-upstream-token",
				"User":        map[string]any{"Id": "password-upstream-user"},
			})
		case r.Method == http.MethodGet && r.URL.Path == "/Users/Me":
			_ = json.NewEncoder(w).Encode(map[string]any{"Id": "apikey-upstream-user"})
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(server.Close)
	return server
}

func loginSecurityRegularUser(t *testing.T, handler http.Handler, username, password string) string {
	t.Helper()
	rr := doJSONRequest(t, handler, http.MethodPost, "/Users/AuthenticateByName", map[string]any{
		"Username": username,
		"Pw":       password,
	}, "")
	if rr.Code != http.StatusOK {
		t.Fatalf("regular user login status = %d, body=%s", rr.Code, rr.Body.String())
	}
	var payload map[string]any
	if err := json.Unmarshal(rr.Body.Bytes(), &payload); err != nil {
		t.Fatalf("decode regular user login: %v", err)
	}
	token, _ := payload["AccessToken"].(string)
	if token == "" {
		t.Fatalf("regular user login missing AccessToken: %#v", payload)
	}
	return token
}

func assertNoCredentialMaterial(t *testing.T, label string, rr *httptest.ResponseRecorder, secrets ...string) {
	t.Helper()
	body := rr.Body.String()
	for _, secret := range secrets {
		if secret != "" && strings.Contains(body, secret) {
			t.Errorf("%s leaked credential value %q in response body: %s", label, secret, body)
		}
	}

	var payload any
	if err := json.Unmarshal(rr.Body.Bytes(), &payload); err != nil {
		return
	}
	forbidden := map[string]bool{
		"password":       true,
		"passwordhash":   true,
		"passwordsecret": true,
		"apikey":         true,
		"pw":             true,
	}
	var walk func(value any, path string)
	walk = func(value any, path string) {
		switch typed := value.(type) {
		case map[string]any:
			for key, child := range typed {
				normalized := strings.ToLower(strings.ReplaceAll(strings.ReplaceAll(key, "_", ""), "-", ""))
				if forbidden[normalized] {
					t.Errorf("%s exposed forbidden credential field %q at %s", label, key, path)
				}
				walk(child, path+"."+key)
			}
		case []any:
			for i, child := range typed {
				walk(child, fmt.Sprintf("%s[%d]", path, i))
			}
		}
	}
	walk(payload, "$")
}

func TestCredentialEditEndpointsRequireAdmin(t *testing.T) {
	upstream := newSecurityCredentialUpstream(t)
	withTempAppPrepared(t, securityCredentialConfig(upstream.URL), nil, func(app *App, handler http.Handler, dir string) {
		user, err := app.UserStore.Create("security-user", securityUserPassword, nil)
		if err != nil {
			t.Fatalf("create regular user: %v", err)
		}
		regularToken := loginSecurityRegularUser(t, handler, user.Username, securityUserPassword)

		for _, tc := range []struct {
			name       string
			target     string
			token      string
			wantStatus int
		}{
			{name: "anonymous upstream list", target: "/admin/api/upstream", wantStatus: http.StatusUnauthorized},
			{name: "anonymous user list", target: "/admin/api/users", wantStatus: http.StatusUnauthorized},
			{name: "regular user upstream list", target: "/admin/api/upstream", token: regularToken, wantStatus: http.StatusForbidden},
			{name: "regular user user list", target: "/admin/api/users", token: regularToken, wantStatus: http.StatusForbidden},
		} {
			t.Run(tc.name, func(t *testing.T) {
				rr := doJSONRequest(t, handler, http.MethodGet, tc.target, nil, tc.token)
				if rr.Code != tc.wantStatus {
					t.Fatalf("status = %d, want %d; body=%s", rr.Code, tc.wantStatus, rr.Body.String())
				}
				assertNoCredentialMaterial(t, tc.name, rr, securityUpstreamPassword, securityUpstreamAPIKey, securityUserPassword)
			})
		}
	})
}

func TestCredentialsStayOutOfNonEditResponses(t *testing.T) {
	upstream := newSecurityCredentialUpstream(t)
	withTempAppPrepared(t, securityCredentialConfig(upstream.URL), nil, func(app *App, handler http.Handler, dir string) {
		user, err := app.UserStore.Create("security-user", securityUserPassword, nil)
		if err != nil {
			t.Fatalf("create regular user: %v", err)
		}
		regularToken := loginSecurityRegularUser(t, handler, user.Username, securityUserPassword)
		adminToken := loginToken(t, handler, "secret")

		calls := []struct {
			name       string
			method     string
			target     string
			body       any
			token      string
			wantStatus int
		}{
			{name: "public system info", method: http.MethodGet, target: "/System/Info/Public", wantStatus: http.StatusOK},
			{name: "public users", method: http.MethodGet, target: "/Users/Public", wantStatus: http.StatusOK},
			{name: "regular user object", method: http.MethodGet, target: "/Users/" + user.ID, token: regularToken, wantStatus: http.StatusOK},
			{name: "regular system info", method: http.MethodGet, target: "/System/Info", token: regularToken, wantStatus: http.StatusOK},
			{name: "admin status", method: http.MethodGet, target: "/admin/api/status", token: adminToken, wantStatus: http.StatusOK},
			{name: "admin logs", method: http.MethodGet, target: "/admin/api/logs?limit=500", token: adminToken, wantStatus: http.StatusOK},
		}
		for _, tc := range calls {
			t.Run(tc.name, func(t *testing.T) {
				rr := doJSONRequest(t, handler, tc.method, tc.target, tc.body, tc.token)
				if rr.Code != tc.wantStatus {
					t.Fatalf("status = %d, want %d; body=%s", rr.Code, tc.wantStatus, rr.Body.String())
				}
				assertNoCredentialMaterial(t, tc.name, rr, securityUpstreamPassword, securityUpstreamAPIKey, securityUserPassword)
			})
		}

		loginRR := doJSONRequest(t, handler, http.MethodPost, "/Users/AuthenticateByName", map[string]any{
			"Username": user.Username,
			"Pw":       securityUserPassword,
		}, "")
		if loginRR.Code != http.StatusOK {
			t.Fatalf("regular authentication response status = %d, body=%s", loginRR.Code, loginRR.Body.String())
		}
		assertNoCredentialMaterial(t, "regular authentication response", loginRR, securityUpstreamPassword, securityUpstreamAPIKey, securityUserPassword)
	})
}

func TestCredentialWriteResponsesDoNotEchoSecrets(t *testing.T) {
	upstream := newSecurityCredentialUpstream(t)
	withTempAppPrepared(t, securityCredentialConfig(upstream.URL), nil, func(app *App, handler http.Handler, dir string) {
		adminToken := loginToken(t, handler, "secret")

		createSecret := "WRITE-USER-PASSWORD-SECRET"
		createRR := doJSONRequest(t, handler, http.MethodPost, "/admin/api/users", map[string]any{
			"username": "write-user",
			"password": createSecret,
		}, adminToken)
		if createRR.Code != http.StatusCreated {
			t.Fatalf("create user status = %d, body=%s", createRR.Code, createRR.Body.String())
		}
		assertNoCredentialMaterial(t, "admin user create response", createRR, createSecret)

		var createPayload map[string]any
		if err := json.Unmarshal(createRR.Body.Bytes(), &createPayload); err != nil {
			t.Fatalf("decode create user response: %v", err)
		}
		userID, _ := createPayload["id"].(string)
		if userID == "" {
			t.Fatalf("create user response missing id: %#v", createPayload)
		}

		updateSecret := "WRITE-USER-PASSWORD-SECRET-2"
		updateRR := doJSONRequest(t, handler, http.MethodPut, "/admin/api/users/"+userID, map[string]any{
			"password": updateSecret,
		}, adminToken)
		if updateRR.Code != http.StatusOK {
			t.Fatalf("update user status = %d, body=%s", updateRR.Code, updateRR.Body.String())
		}
		assertNoCredentialMaterial(t, "admin user update response", updateRR, updateSecret)

		cfg := app.ConfigStore.Snapshot()
		if len(cfg.Upstream) < 1 {
			t.Fatal("security fixture has no upstream")
		}
		upstreamSecret := "UPDATED-UPSTREAM-PASSWORD-SECRET"
		upstreamRR := doJSONRequest(t, handler, http.MethodPut, "/admin/api/upstream/"+cfg.Upstream[0].ID, map[string]any{
			"password": upstreamSecret,
		}, adminToken)
		if upstreamRR.Code != http.StatusOK {
			t.Fatalf("update upstream status = %d, body=%s", upstreamRR.Code, upstreamRR.Body.String())
		}
		assertNoCredentialMaterial(t, "admin upstream update response", upstreamRR, upstreamSecret)
	})
}

func TestAdminCredentialEditListsUseNoStore(t *testing.T) {
	upstream := newSecurityCredentialUpstream(t)
	withTempAppPrepared(t, securityCredentialConfig(upstream.URL), nil, func(app *App, handler http.Handler, dir string) {
		if _, err := app.UserStore.Create("security-user", securityUserPassword, nil); err != nil {
			t.Fatalf("create regular user: %v", err)
		}
		adminToken := loginToken(t, handler, "secret")

		for _, target := range []string{"/admin/api/upstream", "/admin/api/users"} {
			rr := doJSONRequest(t, handler, http.MethodGet, target, nil, adminToken)
			if rr.Code != http.StatusOK {
				t.Fatalf("GET %s status = %d, body=%s", target, rr.Code, rr.Body.String())
			}
			if got := rr.Header().Get("Cache-Control"); got != "no-store" {
				t.Errorf("GET %s Cache-Control = %q, want no-store", target, got)
			}
		}
	})
}
