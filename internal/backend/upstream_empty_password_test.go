package backend

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestUpstreamClientSendsExplicitEmptyPassword(t *testing.T) {
	var received map[string]any
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost || r.URL.Path != "/Users/AuthenticateByName" {
			http.NotFound(w, r)
			return
		}
		if err := json.NewDecoder(r.Body).Decode(&received); err != nil {
			t.Fatalf("decode upstream login body: %v", err)
		}
		_ = json.NewEncoder(w).Encode(map[string]any{
			"AccessToken": "empty-password-token",
			"User":        map[string]any{"Id": "empty-password-user"},
		})
	}))
	defer upstream.Close()

	cfg := Config{Timeouts: TimeoutsConfig{API: 30000, Login: 10000, HealthCheck: 10000}}
	client := newUpstreamClient(cfg, UpstreamConfig{
		Name:         "empty-password-upstream",
		URL:          upstream.URL,
		Username:     "alice",
		Password:     "",
		SpoofClient:  "none",
		PlaybackMode: "proxy",
	}, 0, nil)

	client.Login(context.Background(), nil, nil)

	if received == nil {
		t.Fatal("upstream never received AuthenticateByName request")
	}
	if got, ok := received["Username"].(string); !ok || got != "alice" {
		t.Fatalf("Username = %#v, want alice", received["Username"])
	}
	pw, exists := received["Pw"]
	if !exists {
		t.Fatal("AuthenticateByName body omitted Pw; want explicit empty string")
	}
	if got, ok := pw.(string); !ok || got != "" {
		t.Fatalf("Pw = %#v, want explicit empty string", pw)
	}
	if !client.IsOnline() {
		t.Fatalf("client is offline after successful empty-password login: %+v", client.snapshot())
	}
	state := client.snapshot()
	if state.AccessToken != "empty-password-token" || state.UserID != "empty-password-user" {
		t.Fatalf("login state = token %q user %q, want empty-password-token/empty-password-user", state.AccessToken, state.UserID)
	}
}

func TestAdminUpstreamCreateAcceptsEmptyPassword(t *testing.T) {
	loginBodies := make(chan map[string]any, 4)
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost || r.URL.Path != "/Users/AuthenticateByName" {
			http.NotFound(w, r)
			return
		}
		var body map[string]any
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			t.Errorf("decode upstream login body: %v", err)
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		loginBodies <- body
		_ = json.NewEncoder(w).Encode(map[string]any{
			"AccessToken": "created-empty-token",
			"User":        map[string]any{"Id": "created-empty-user"},
		})
	}))
	defer upstream.Close()

	withTempApp(t, func(app *App, handler http.Handler) {
		token := loginToken(t, handler, "secret")
		if got := len(app.ConfigStore.Snapshot().Upstream); got != 0 {
			t.Fatalf("initial upstream count = %d, want 0", got)
		}

		rr := doJSONRequest(t, handler, http.MethodPost, "/admin/api/upstream", map[string]any{
			"name":         "empty-password-upstream",
			"url":          upstream.URL,
			"authType":     "password",
			"username":     "alice",
			"password":     "",
			"playbackMode": "proxy",
			"spoofClient":  "none",
		}, token)
		if rr.Code != http.StatusOK {
			t.Fatalf("create empty-password upstream status = %d, body=%s", rr.Code, rr.Body.String())
		}

		var loginBody map[string]any
		select {
		case loginBody = <-loginBodies:
		default:
			t.Fatal("admin create never validated credentials with AuthenticateByName")
		}
		if got, _ := loginBody["Username"].(string); got != "alice" {
			t.Fatalf("upstream Username = %#v, want alice", loginBody["Username"])
		}
		pw, exists := loginBody["Pw"]
		if !exists {
			t.Fatal("upstream AuthenticateByName body omitted Pw")
		}
		if got, ok := pw.(string); !ok || got != "" {
			t.Fatalf("upstream Pw = %#v, want explicit empty string", pw)
		}

		cfg := app.ConfigStore.Snapshot()
		if len(cfg.Upstream) != 1 {
			t.Fatalf("saved upstream count = %d, want 1", len(cfg.Upstream))
		}
		saved := cfg.Upstream[0]
		if saved.Username != "alice" || saved.Password != "" || saved.APIKey != "" {
			t.Fatalf("saved credentials = username:%q password:%q apiKey:%q, want alice/empty/empty", saved.Username, saved.Password, saved.APIKey)
		}
	})
}

func TestAdminUpstreamUpdateClearsPasswordAndReauthenticatesEmpty(t *testing.T) {
	loginBodies := make(chan map[string]any, 8)
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost || r.URL.Path != "/Users/AuthenticateByName" {
			http.NotFound(w, r)
			return
		}
		var body map[string]any
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			t.Errorf("decode upstream login body: %v", err)
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		loginBodies <- body
		_ = json.NewEncoder(w).Encode(map[string]any{
			"AccessToken": "updated-empty-token",
			"User":        map[string]any{"Id": "updated-empty-user"},
		})
	}))
	defer upstream.Close()

	withTempAppConfig(t, singleUpstreamConfig(upstream.URL), func(app *App, handler http.Handler) {
		for {
			select {
			case <-loginBodies:
				continue
			default:
				goto drained
			}
		}
	drained:

		token := loginToken(t, handler, "secret")
		rr := doJSONRequest(t, handler, http.MethodPut, "/admin/api/upstream/0", map[string]any{
			"name":         "A",
			"url":          upstream.URL,
			"authType":     "password",
			"username":     "u1",
			"password":     "",
			"playbackMode": "proxy",
		}, token)
		if rr.Code != http.StatusOK {
			t.Fatalf("update to empty password status = %d, body=%s", rr.Code, rr.Body.String())
		}

		var validationBody map[string]any
		select {
		case validationBody = <-loginBodies:
		default:
			t.Fatal("admin update never revalidated credentials with AuthenticateByName")
		}
		if got, _ := validationBody["Username"].(string); got != "u1" {
			t.Fatalf("upstream Username = %#v, want u1", validationBody["Username"])
		}
		pw, exists := validationBody["Pw"]
		if !exists {
			t.Fatal("upstream AuthenticateByName body omitted Pw")
		}
		if got, ok := pw.(string); !ok || got != "" {
			t.Fatalf("upstream Pw = %#v, want explicit empty string", pw)
		}

		cfg := app.ConfigStore.Snapshot()
		if len(cfg.Upstream) != 1 {
			t.Fatalf("saved upstream count = %d, want 1", len(cfg.Upstream))
		}
		saved := cfg.Upstream[0]
		if saved.Username != "u1" || saved.Password != "" || saved.APIKey != "" {
			t.Fatalf("saved credentials = username:%q password:%q apiKey:%q, want u1/empty/empty", saved.Username, saved.Password, saved.APIKey)
		}
	})
}

func TestAdminUpstreamListReturnsEditableCredentialsNoStore(t *testing.T) {
	newServer := func(t *testing.T) *httptest.Server {
		t.Helper()
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			switch {
			case r.Method == http.MethodPost && r.URL.Path == "/Users/AuthenticateByName":
				_ = json.NewEncoder(w).Encode(map[string]any{"AccessToken": "pw-token", "User": map[string]any{"Id": "pw-user"}})
			case r.Method == http.MethodGet && r.URL.Path == "/Users/Me":
				_ = json.NewEncoder(w).Encode(map[string]any{"Id": "apikey-user"})
			default:
				http.NotFound(w, r)
			}
		}))
		t.Cleanup(srv.Close)
		return srv
	}

	assertList := func(t *testing.T, config string, wantUsername, wantPassword, wantAPIKey, wantAuthType string) {
		t.Helper()
		withTempAppConfig(t, config, func(app *App, handler http.Handler) {
			token := loginToken(t, handler, "secret")
			rr := doJSONRequest(t, handler, http.MethodGet, "/admin/api/upstream", nil, token)
			if rr.Code != http.StatusOK {
				t.Fatalf("list status = %d, body=%s", rr.Code, rr.Body.String())
			}
			if got := rr.Header().Get("Cache-Control"); got != "no-store" {
				t.Errorf("Cache-Control = %q, want no-store", got)
			}
			var rows []map[string]any
			if err := json.Unmarshal(rr.Body.Bytes(), &rows); err != nil {
				t.Fatalf("decode upstream list: %v", err)
			}
			if len(rows) != 1 {
				t.Fatalf("upstream rows = %d, want 1", len(rows))
			}
			row := rows[0]
			if got, _ := row["authType"].(string); got != wantAuthType {
				t.Errorf("authType = %q, want %q", got, wantAuthType)
			}
			if got, _ := row["username"].(string); got != wantUsername {
				t.Errorf("username = %q, want %q", got, wantUsername)
			}
			password, passwordExists := row["password"]
			if !passwordExists {
				t.Errorf("password field missing, want explicit %q", wantPassword)
			} else if got, ok := password.(string); !ok || got != wantPassword {
				t.Errorf("password = %#v, want %q", password, wantPassword)
			}
			apiKey, apiKeyExists := row["apiKey"]
			if !apiKeyExists {
				t.Errorf("apiKey field missing, want explicit %q", wantAPIKey)
			} else if got, ok := apiKey.(string); !ok || got != wantAPIKey {
				t.Errorf("apiKey = %#v, want %q", apiKey, wantAPIKey)
			}
		})
	}

	t.Run("password mode returns current nonempty password", func(t *testing.T) {
		upstream := newServer(t)
		assertList(t, singleUpstreamConfig(upstream.URL), "u1", "p1", "", "password")
	})

	t.Run("password mode returns explicit empty password", func(t *testing.T) {
		upstream := newServer(t)
		config := fmt.Sprintf(`server:
  port: 8096
  name: "Test"
  id: "svr"
admin:
  username: "admin"
  password: "secret"
playback:
  mode: "proxy"
timeouts:
  api: 30000
  global: 15000
  login: 10000
  healthCheck: 10000
  healthInterval: 60000
proxies: []
upstream:
  - name: "A"
    url: %q
    username: "empty-user"
    password: ""
`, upstream.URL)
		assertList(t, config, "empty-user", "", "", "password")
	})

	t.Run("apiKey mode returns current key and clears inactive fields", func(t *testing.T) {
		upstream := newServer(t)
		assertList(t, apiKeyUpstreamConfig(upstream.URL, "KEY123"), "", "", "KEY123", "apiKey")
	})
}

func TestEmptyPasswordPersistsAcrossConfigReloadAndRelogin(t *testing.T) {
	loginBodies := make(chan map[string]any, 8)
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost || r.URL.Path != "/Users/AuthenticateByName" {
			http.NotFound(w, r)
			return
		}
		var body map[string]any
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			t.Errorf("decode upstream login body: %v", err)
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		loginBodies <- body
		_ = json.NewEncoder(w).Encode(map[string]any{
			"AccessToken": "reloaded-empty-token",
			"User":        map[string]any{"Id": "reloaded-empty-user"},
		})
	}))
	defer upstream.Close()

	withTempAppConfig(t, singleUpstreamConfig(upstream.URL), func(app *App, _ http.Handler) {
		for {
			select {
			case <-loginBodies:
				continue
			default:
				goto drained
			}
		}
	drained:

		cfg := app.ConfigStore.Snapshot()
		if len(cfg.Upstream) != 1 {
			t.Fatalf("initial upstream count = %d, want 1", len(cfg.Upstream))
		}
		cfg.Upstream[0].Username = "u1"
		cfg.Upstream[0].Password = ""
		cfg.Upstream[0].APIKey = ""
		app.ConfigStore.Replace(cfg)
		if err := app.ConfigStore.Save(); err != nil {
			t.Fatalf("save empty-password config: %v", err)
		}

		reloadedStore, err := LoadConfigStore()
		if err != nil {
			t.Fatalf("reload config store: %v", err)
		}
		reloaded := reloadedStore.Snapshot()
		if len(reloaded.Upstream) != 1 {
			t.Fatalf("reloaded upstream count = %d, want 1", len(reloaded.Upstream))
		}
		saved := reloaded.Upstream[0]
		if saved.Username != "u1" || saved.Password != "" || saved.APIKey != "" {
			t.Fatalf("reloaded credentials = username:%q password:%q apiKey:%q, want u1/empty/empty", saved.Username, saved.Password, saved.APIKey)
		}

		client := newUpstreamClient(reloaded, saved, 0, nil)
		client.Login(context.Background(), nil, nil)

		var loginBody map[string]any
		select {
		case loginBody = <-loginBodies:
		default:
			t.Fatal("reloaded client never called AuthenticateByName")
		}
		if got, _ := loginBody["Username"].(string); got != "u1" {
			t.Fatalf("reloaded Username = %#v, want u1", loginBody["Username"])
		}
		pw, exists := loginBody["Pw"]
		if !exists {
			t.Fatal("reloaded AuthenticateByName body omitted Pw")
		}
		if got, ok := pw.(string); !ok || got != "" {
			t.Fatalf("reloaded Pw = %#v, want explicit empty string", pw)
		}
		if !client.IsOnline() {
			t.Fatalf("reloaded client is offline after empty-password login: %+v", client.snapshot())
		}
	})
}
