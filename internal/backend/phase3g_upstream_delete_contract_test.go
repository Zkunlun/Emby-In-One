package backend

import (
	"net/http"
	"testing"
)

func TestPhase3GDeletingUpstreamReleasesGrantsAndRevokesTokens(t *testing.T) {
	upstream := phase1AUpstreamStub(t)
	defer upstream.Close()

	withTempAppConfig(t, phase1AAuthorizationSlotConfig(upstream.URL), func(app *App, handler http.Handler) {
		adminToken := loginTokenAs(t, handler, "admin", "secret")
		createRR := phase1ACreateUser(t, handler, adminToken, "phase3g-user", []string{"server-a"})
		if createRR.Code != http.StatusCreated {
			t.Fatalf("create user: status=%d body=%s", createRR.Code, createRR.Body.String())
		}
		userID := phase1AUserID(t, createRR)
		userToken := loginTokenAs(t, handler, "phase3g-user", "password123")

		if got := app.UserStore.CountUsersForServer("server-a"); got != 1 {
			t.Fatalf("assigned users before delete = %d, want 1", got)
		}
		if rr := doAuthJSON(t, handler, http.MethodGet, "/System/Info", nil, userToken); rr.Code != http.StatusOK {
			t.Fatalf("user token before delete: status=%d body=%s", rr.Code, rr.Body.String())
		}

		deleteRR := doAuthJSON(t, handler, http.MethodDelete, "/admin/api/upstream/server-a", nil, adminToken)
		if deleteRR.Code != http.StatusOK {
			t.Fatalf("delete upstream: status=%d body=%s", deleteRR.Code, deleteRR.Body.String())
		}

		if got := app.UserStore.CountUsersForServer("server-a"); got != 0 {
			t.Fatalf("assigned users after delete = %d, want 0", got)
		}
		user := app.UserStore.Get(userID)
		if user == nil {
			t.Fatal("user missing after upstream delete")
		}
		if len(user.AllowedServers) != 0 {
			t.Fatalf("allowed servers after upstream delete = %v, want []", user.AllowedServers)
		}
		if rr := doAuthJSON(t, handler, http.MethodGet, "/System/Info", nil, userToken); rr.Code != http.StatusUnauthorized {
			t.Fatalf("stale user token after upstream delete: status=%d, want 401 body=%s", rr.Code, rr.Body.String())
		}

		cfg := app.ConfigStore.Snapshot()
		for _, configured := range cfg.Upstream {
			if configured.ID == "server-a" {
				t.Fatal("deleted upstream server-a still present in config")
			}
		}
	})
}
