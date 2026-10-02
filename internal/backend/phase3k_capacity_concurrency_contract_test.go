package backend

import (
	"net/http"
	"sync"
	"testing"
)

func TestPhase3KConcurrentLimitLowerAndGrantPreserveCapacityInvariant(t *testing.T) {
	upstream := phase1AUpstreamStub(t)
	defer upstream.Close()

	withTempAppConfig(t, phase1AAuthorizationSlotConfig(upstream.URL), func(app *App, handler http.Handler) {
		adminToken := loginTokenAs(t, handler, "admin", "secret")
		seed := phase1ACreateUser(t, handler, adminToken, "phase3k-seed", []string{"server-a"})
		if seed.Code != http.StatusCreated {
			t.Fatalf("seed user: status=%d body=%s", seed.Code, seed.Body.String())
		}

		start := make(chan struct{})
		var wg sync.WaitGroup
		wg.Add(2)
		statuses := make(chan int, 2)

		go func() {
			defer wg.Done()
			<-start
			rr := doAuthJSON(t, handler, http.MethodPut, "/admin/api/upstream/server-a", map[string]any{
				"maxConcurrent": 1,
			}, adminToken)
			statuses <- rr.Code
		}()
		go func() {
			defer wg.Done()
			<-start
			rr := phase1ACreateUser(t, handler, adminToken, "phase3k-racer", []string{"server-a"})
			statuses <- rr.Code
		}()

		close(start)
		wg.Wait()
		close(statuses)

		successes := 0
		conflicts := 0
		for status := range statuses {
			switch status {
			case http.StatusOK, http.StatusCreated:
				successes++
			case http.StatusConflict:
				conflicts++
			default:
				t.Fatalf("unexpected concurrent mutation status=%d", status)
			}
		}
		if successes != 1 || conflicts != 1 {
			t.Fatalf("concurrent lower/grant results: successes=%d conflicts=%d, want 1/1", successes, conflicts)
		}

		limit := 0
		for _, configured := range app.ConfigStore.Snapshot().Upstream {
			if configured.ID == "server-a" {
				limit = configured.MaxConcurrent
				break
			}
		}
		assigned := app.UserStore.CountUsersForServer("server-a")
		if limit > 0 && assigned > limit {
			t.Fatalf("capacity invariant violated after concurrent lower/grant: assigned=%d limit=%d", assigned, limit)
		}
	})
}

func TestPhase3KConcurrentDeleteAndGrantCannotLeaveOrphanGrant(t *testing.T) {
	upstream := phase1AUpstreamStub(t)
	defer upstream.Close()

	withTempAppConfig(t, phase1AAuthorizationSlotConfig(upstream.URL), func(app *App, handler http.Handler) {
		adminToken := loginTokenAs(t, handler, "admin", "secret")
		start := make(chan struct{})
		var wg sync.WaitGroup
		wg.Add(2)
		deleteStatus := 0
		createStatus := 0

		go func() {
			defer wg.Done()
			<-start
			deleteStatus = doAuthJSON(t, handler, http.MethodDelete, "/admin/api/upstream/server-a", nil, adminToken).Code
		}()
		go func() {
			defer wg.Done()
			<-start
			createStatus = phase1ACreateUser(t, handler, adminToken, "phase3k-delete-racer", []string{"server-a"}).Code
		}()

		close(start)
		wg.Wait()

		if deleteStatus != http.StatusOK {
			t.Fatalf("concurrent upstream delete status=%d, want 200", deleteStatus)
		}
		if createStatus != http.StatusCreated && createStatus != http.StatusBadRequest {
			t.Fatalf("concurrent user create status=%d, want 201 before delete or 400 after delete", createStatus)
		}
		for _, configured := range app.ConfigStore.Snapshot().Upstream {
			if configured.ID == "server-a" {
				t.Fatal("server-a still present after successful concurrent delete")
			}
		}
		if got := app.UserStore.CountUsersForServer("server-a"); got != 0 {
			t.Fatalf("deleted upstream left %d orphan authorization grant(s)", got)
		}
	})
}
