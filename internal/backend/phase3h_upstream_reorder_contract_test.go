package backend

import (
	"encoding/json"
	"fmt"
	"net/http"
	"testing"
)

func phase3HReorderConfig(urlA, urlB string) string {
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
  - id: "server-b"
    name: "B"
    url: %q
    username: "u2"
    password: "p2"
    maxConcurrent: 3
`, urlA, urlB)
}

func TestPhase3HReorderPreservesServerIDAssignmentsAndCapacity(t *testing.T) {
	upstreamA := phase1AUpstreamStub(t)
	defer upstreamA.Close()
	upstreamB := phase1AUpstreamStub(t)
	defer upstreamB.Close()

	withTempAppConfig(t, phase3HReorderConfig(upstreamA.URL, upstreamB.URL), func(app *App, handler http.Handler) {
		adminToken := loginTokenAs(t, handler, "admin", "secret")
		for _, username := range []string{"phase3h-a", "phase3h-b"} {
			rr := phase1ACreateUser(t, handler, adminToken, username, []string{"server-a"})
			if rr.Code != http.StatusCreated {
				t.Fatalf("create %s: status=%d body=%s", username, rr.Code, rr.Body.String())
			}
		}

		if got := app.UserStore.CountUsersForServer("server-a"); got != 2 {
			t.Fatalf("server-a assigned users before reorder = %d, want 2", got)
		}
		if got := app.UserStore.CountUsersForServer("server-b"); got != 0 {
			t.Fatalf("server-b assigned users before reorder = %d, want 0", got)
		}

		reorderRR := doAuthJSON(t, handler, http.MethodPost, "/admin/api/upstream/reorder", map[string]any{
			"fromIndex": 0,
			"toIndex":   1,
		}, adminToken)
		if reorderRR.Code != http.StatusOK {
			t.Fatalf("reorder upstreams: status=%d body=%s", reorderRR.Code, reorderRR.Body.String())
		}

		cfg := app.ConfigStore.Snapshot()
		if len(cfg.Upstream) != 2 || cfg.Upstream[0].ID != "server-b" || cfg.Upstream[1].ID != "server-a" {
			t.Fatalf("upstream order after reorder = %#v, want [server-b server-a]", cfg.Upstream)
		}
		if got := app.UserStore.CountUsersForServer("server-a"); got != 2 {
			t.Fatalf("server-a assigned users after reorder = %d, want 2", got)
		}
		if got := app.UserStore.CountUsersForServer("server-b"); got != 0 {
			t.Fatalf("server-b assigned users after reorder = %d, want 0", got)
		}
		for _, user := range app.UserStore.List() {
			if len(user.AllowedServers) != 1 || user.AllowedServers[0] != "server-a" {
				t.Fatalf("user %s grants changed after reorder: %v", user.Username, user.AllowedServers)
			}
		}

		listRR := doAuthJSON(t, handler, http.MethodGet, "/admin/api/upstream", nil, adminToken)
		if listRR.Code != http.StatusOK {
			t.Fatalf("list upstreams after reorder: status=%d body=%s", listRR.Code, listRR.Body.String())
		}
		var upstreams []map[string]any
		if err := json.Unmarshal(listRR.Body.Bytes(), &upstreams); err != nil {
			t.Fatalf("decode upstream list: %v body=%s", err, listRR.Body.String())
		}
		if len(upstreams) != 2 {
			t.Fatalf("upstream list length = %d, want 2", len(upstreams))
		}
		assignedByID := make(map[string]int, len(upstreams))
		for _, upstream := range upstreams {
			id, _ := upstream["id"].(string)
			assigned, _ := upstream["assignedUsers"].(float64)
			assignedByID[id] = int(assigned)
		}
		if assignedByID["server-a"] != 2 || assignedByID["server-b"] != 0 {
			t.Fatalf("assignedUsers after reorder = %v, want server-a=2 server-b=0", assignedByID)
		}

		capacityRR := doAuthJSON(t, handler, http.MethodPut, "/admin/api/upstream/server-a", map[string]any{
			"maxConcurrent": 1,
		}, adminToken)
		if capacityRR.Code != http.StatusConflict {
			t.Fatalf("server-a capacity after reorder: status=%d, want 409 body=%s", capacityRR.Code, capacityRR.Body.String())
		}
		if code := phase1GErrorCode(t, capacityRR); code != "UPSTREAM_CAPACITY_BELOW_ASSIGNED" {
			t.Fatalf("server-a capacity code after reorder = %q, want UPSTREAM_CAPACITY_BELOW_ASSIGNED", code)
		}
	})
}
