package backend

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"
)

func phase1AAuthorizationSlotConfig(url string) string {
	return fmt.Sprintf(`server:
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
  - id: "server-a"
    name: "A"
    url: %q
    username: "u1"
    password: "p1"
    maxConcurrent: 2
`, url)
}

func phase1AUpstreamStub(t *testing.T) *httptest.Server {
	t.Helper()
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodPost && r.URL.Path == "/Users/AuthenticateByName":
			_ = json.NewEncoder(w).Encode(map[string]any{
				"AccessToken": "tok-a",
				"User":        map[string]any{"Id": "user-a"},
			})
		default:
			http.NotFound(w, r)
		}
	}))
}

func phase1ACreateUser(t *testing.T, handler http.Handler, adminToken, username string, allowedServers []string) *httptest.ResponseRecorder {
	t.Helper()
	return doAuthJSON(t, handler, http.MethodPost, "/admin/api/users", map[string]any{
		"username":       username,
		"password":       "password123",
		"allowedServers": allowedServers,
	}, adminToken)
}

func phase1AUserID(t *testing.T, rr *httptest.ResponseRecorder) string {
	t.Helper()
	var payload map[string]any
	if err := json.Unmarshal(rr.Body.Bytes(), &payload); err != nil {
		t.Fatalf("decode user response: %v body=%s", err, rr.Body.String())
	}
	id, _ := payload["id"].(string)
	if id == "" {
		t.Fatalf("user response missing id: %s", rr.Body.String())
	}
	return id
}

func phase1ACountAssignedUsers(app *App, serverID string) int {
	count := 0
	for _, user := range app.UserStore.List() {
		for _, allowed := range user.AllowedServers {
			if allowed == serverID {
				count++
				break
			}
		}
	}
	return count
}

func TestPhase1AAuthorizationSlotContract(t *testing.T) {
	t.Run("third user cannot claim a full server", func(t *testing.T) {
		upstream := phase1AUpstreamStub(t)
		defer upstream.Close()

		withTempAppConfig(t, phase1AAuthorizationSlotConfig(upstream.URL), func(app *App, handler http.Handler) {
			adminToken := loginTokenAs(t, handler, "admin", "secret")
			for _, username := range []string{"alice", "bob"} {
				rr := phase1ACreateUser(t, handler, adminToken, username, []string{"server-a"})
				if rr.Code != http.StatusCreated {
					t.Fatalf("create %s: status=%d body=%s", username, rr.Code, rr.Body.String())
				}
			}

			rr := phase1ACreateUser(t, handler, adminToken, "charlie", []string{"server-a"})
			if rr.Code == http.StatusCreated {
				t.Fatalf("third assignment unexpectedly succeeded: status=%d body=%s", rr.Code, rr.Body.String())
			}
			if got := phase1ACountAssignedUsers(app, "server-a"); got != 2 {
				t.Fatalf("assigned users = %d, want 2 after rejected third assignment", got)
			}
		})
	})

	t.Run("full server does not block user creation without that grant", func(t *testing.T) {
		upstream := phase1AUpstreamStub(t)
		defer upstream.Close()

		withTempAppConfig(t, phase1AAuthorizationSlotConfig(upstream.URL), func(app *App, handler http.Handler) {
			adminToken := loginTokenAs(t, handler, "admin", "secret")
			for _, username := range []string{"alice", "bob"} {
				rr := phase1ACreateUser(t, handler, adminToken, username, []string{"server-a"})
				if rr.Code != http.StatusCreated {
					t.Fatalf("create %s: status=%d body=%s", username, rr.Code, rr.Body.String())
				}
			}

			rr := phase1ACreateUser(t, handler, adminToken, "charlie", []string{})
			if rr.Code != http.StatusCreated {
				t.Fatalf("create unassigned user: status=%d body=%s", rr.Code, rr.Body.String())
			}
			if got := phase1ACountAssignedUsers(app, "server-a"); got != 2 {
				t.Fatalf("assigned users = %d, want 2", got)
			}
		})
	})

	t.Run("removing a grant releases the slot", func(t *testing.T) {
		upstream := phase1AUpstreamStub(t)
		defer upstream.Close()

		withTempAppConfig(t, phase1AAuthorizationSlotConfig(upstream.URL), func(app *App, handler http.Handler) {
			adminToken := loginTokenAs(t, handler, "admin", "secret")
			alice := phase1ACreateUser(t, handler, adminToken, "alice", []string{"server-a"})
			bob := phase1ACreateUser(t, handler, adminToken, "bob", []string{"server-a"})
			if alice.Code != http.StatusCreated || bob.Code != http.StatusCreated {
				t.Fatalf("seed users failed: alice=%d bob=%d", alice.Code, bob.Code)
			}
			bobID := phase1AUserID(t, bob)

			rr := doAuthJSON(t, handler, http.MethodPut, "/admin/api/users/"+bobID,
				map[string]any{"allowedServers": []string{}}, adminToken)
			if rr.Code != http.StatusOK {
				t.Fatalf("remove grant: status=%d body=%s", rr.Code, rr.Body.String())
			}
			if got := phase1ACountAssignedUsers(app, "server-a"); got != 1 {
				t.Fatalf("assigned users after removing grant = %d, want 1", got)
			}

			rr = phase1ACreateUser(t, handler, adminToken, "charlie", []string{"server-a"})
			if rr.Code != http.StatusCreated {
				t.Fatalf("replacement assignment: status=%d body=%s", rr.Code, rr.Body.String())
			}
			if got := phase1ACountAssignedUsers(app, "server-a"); got != 2 {
				t.Fatalf("assigned users after replacement = %d, want 2", got)
			}
		})
	})

	t.Run("deleting a user releases the slot", func(t *testing.T) {
		upstream := phase1AUpstreamStub(t)
		defer upstream.Close()

		withTempAppConfig(t, phase1AAuthorizationSlotConfig(upstream.URL), func(app *App, handler http.Handler) {
			adminToken := loginTokenAs(t, handler, "admin", "secret")
			alice := phase1ACreateUser(t, handler, adminToken, "alice", []string{"server-a"})
			bob := phase1ACreateUser(t, handler, adminToken, "bob", []string{"server-a"})
			if alice.Code != http.StatusCreated || bob.Code != http.StatusCreated {
				t.Fatalf("seed users failed: alice=%d bob=%d", alice.Code, bob.Code)
			}
			bobID := phase1AUserID(t, bob)

			rr := doAuthJSON(t, handler, http.MethodDelete, "/admin/api/users/"+bobID, nil, adminToken)
			if rr.Code != http.StatusOK {
				t.Fatalf("delete bob: status=%d body=%s", rr.Code, rr.Body.String())
			}
			if got := phase1ACountAssignedUsers(app, "server-a"); got != 1 {
				t.Fatalf("assigned users after delete = %d, want 1", got)
			}

			rr = phase1ACreateUser(t, handler, adminToken, "charlie", []string{"server-a"})
			if rr.Code != http.StatusCreated {
				t.Fatalf("replacement assignment: status=%d body=%s", rr.Code, rr.Body.String())
			}
		})
	})

	t.Run("disabled user keeps the slot", func(t *testing.T) {
		upstream := phase1AUpstreamStub(t)
		defer upstream.Close()

		withTempAppConfig(t, phase1AAuthorizationSlotConfig(upstream.URL), func(app *App, handler http.Handler) {
			adminToken := loginTokenAs(t, handler, "admin", "secret")
			alice := phase1ACreateUser(t, handler, adminToken, "alice", []string{"server-a"})
			bob := phase1ACreateUser(t, handler, adminToken, "bob", []string{"server-a"})
			if alice.Code != http.StatusCreated || bob.Code != http.StatusCreated {
				t.Fatalf("seed users failed: alice=%d bob=%d", alice.Code, bob.Code)
			}
			bobID := phase1AUserID(t, bob)

			rr := doAuthJSON(t, handler, http.MethodPut, "/admin/api/users/"+bobID,
				map[string]any{"enabled": false}, adminToken)
			if rr.Code != http.StatusOK {
				t.Fatalf("disable bob: status=%d body=%s", rr.Code, rr.Body.String())
			}
			if got := phase1ACountAssignedUsers(app, "server-a"); got != 2 {
				t.Fatalf("disabled user released slot: assigned=%d want=2", got)
			}

			rr = phase1ACreateUser(t, handler, adminToken, "charlie", []string{"server-a"})
			if rr.Code == http.StatusCreated {
				t.Fatalf("third assignment succeeded after disabling an assigned user: %s", rr.Body.String())
			}
		})
	})

	t.Run("editing an existing full-server grant does not consume another slot", func(t *testing.T) {
		upstream := phase1AUpstreamStub(t)
		defer upstream.Close()

		withTempAppConfig(t, phase1AAuthorizationSlotConfig(upstream.URL), func(app *App, handler http.Handler) {
			adminToken := loginTokenAs(t, handler, "admin", "secret")
			alice := phase1ACreateUser(t, handler, adminToken, "alice", []string{"server-a"})
			bob := phase1ACreateUser(t, handler, adminToken, "bob", []string{"server-a"})
			if alice.Code != http.StatusCreated || bob.Code != http.StatusCreated {
				t.Fatalf("seed users failed: alice=%d bob=%d", alice.Code, bob.Code)
			}
			aliceID := phase1AUserID(t, alice)

			rr := doAuthJSON(t, handler, http.MethodPut, "/admin/api/users/"+aliceID,
				map[string]any{"username": "alice-renamed", "allowedServers": []string{"server-a"}}, adminToken)
			if rr.Code != http.StatusOK {
				t.Fatalf("edit existing grant: status=%d body=%s", rr.Code, rr.Body.String())
			}
			if got := phase1ACountAssignedUsers(app, "server-a"); got != 2 {
				t.Fatalf("assigned users after existing-grant edit = %d, want 2", got)
			}
		})
	})
}
