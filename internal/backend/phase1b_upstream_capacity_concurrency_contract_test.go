package backend

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
)

func phase1BConcurrentCreate(handler http.Handler, adminToken, username string, start <-chan struct{}, results chan<- *httptest.ResponseRecorder, wg *sync.WaitGroup) {
	defer wg.Done()
	payload, _ := json.Marshal(map[string]any{
		"username":       username,
		"password":       "password123",
		"allowedServers": []string{"server-a"},
	})
	<-start
	req := httptest.NewRequest(http.MethodPost, "/admin/api/users", bytes.NewReader(payload))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-Emby-Token", adminToken)
	rr := httptest.NewRecorder()
	handler.ServeHTTP(rr, req)
	results <- rr
}

func TestPhase1BAuthorizationSlotLastCapacityIsAtomic(t *testing.T) {
	upstream := phase1AUpstreamStub(t)
	defer upstream.Close()

	withTempAppConfig(t, phase1AAuthorizationSlotConfig(upstream.URL), func(app *App, handler http.Handler) {
		adminToken := loginTokenAs(t, handler, "admin", "secret")

		// Occupy one of the two slots first. Bob and Charlie then race for the
		// single remaining slot. The capacity check and user_servers mutation must
		// eventually be one atomic operation: exactly one concurrent request wins.
		alice := phase1ACreateUser(t, handler, adminToken, "alice", []string{"server-a"})
		if alice.Code != http.StatusCreated {
			t.Fatalf("create alice: status=%d body=%s", alice.Code, alice.Body.String())
		}

		start := make(chan struct{})
		results := make(chan *httptest.ResponseRecorder, 2)
		var wg sync.WaitGroup
		for _, username := range []string{"bob", "charlie"} {
			wg.Add(1)
			go phase1BConcurrentCreate(handler, adminToken, username, start, results, &wg)
		}
		close(start)
		wg.Wait()
		close(results)

		created := 0
		rejected := 0
		for rr := range results {
			if rr.Code == http.StatusCreated {
				created++
			} else {
				rejected++
			}
		}

		if created != 1 || rejected != 1 {
			t.Fatalf("concurrent last-slot results: created=%d rejected=%d, want exactly one of each", created, rejected)
		}
		if got := phase1ACountAssignedUsers(app, "server-a"); got != 2 {
			t.Fatalf("assigned users after last-slot race = %d, want 2", got)
		}
	})
}
