package backend

import (
	"encoding/json"
	"net/http"
	"testing"
)

func phase3IUpstreamByID(t *testing.T, handler http.Handler, adminToken, serverID string) map[string]any {
	t.Helper()
	rr := doAuthJSON(t, handler, http.MethodGet, "/admin/api/upstream", nil, adminToken)
	if rr.Code != http.StatusOK {
		t.Fatalf("list upstreams: status=%d body=%s", rr.Code, rr.Body.String())
	}
	var upstreams []map[string]any
	if err := json.Unmarshal(rr.Body.Bytes(), &upstreams); err != nil {
		t.Fatalf("decode upstream list: %v body=%s", err, rr.Body.String())
	}
	for _, upstream := range upstreams {
		if id, _ := upstream["id"].(string); id == serverID {
			return upstream
		}
	}
	t.Fatalf("upstream %q missing from response: %v", serverID, upstreams)
	return nil
}

func TestPhase3IAdminUpstreamCapacityDataContract(t *testing.T) {
	upstream := phase1AUpstreamStub(t)
	defer upstream.Close()

	withTempAppConfig(t, phase1AAuthorizationSlotConfig(upstream.URL), func(app *App, handler http.Handler) {
		adminToken := loginTokenAs(t, handler, "admin", "secret")
		createRR := phase1ACreateUser(t, handler, adminToken, "phase3i-user", []string{"server-a"})
		if createRR.Code != http.StatusCreated {
			t.Fatalf("create user: status=%d body=%s", createRR.Code, createRR.Body.String())
		}

		entry := phase3IUpstreamByID(t, handler, adminToken, "server-a")
		if got, _ := entry["maxConcurrent"].(float64); got != 2 {
			t.Fatalf("maxConcurrent = %v, want 2", entry["maxConcurrent"])
		}
		if got, _ := entry["assignedUsers"].(float64); got != 1 {
			t.Fatalf("assignedUsers = %v, want 1", entry["assignedUsers"])
		}

		updateRR := doAuthJSON(t, handler, http.MethodPut, "/admin/api/upstream/server-a", map[string]any{
			"maxConcurrent": 0,
		}, adminToken)
		if updateRR.Code != http.StatusOK {
			t.Fatalf("set unlimited maxConcurrent: status=%d body=%s", updateRR.Code, updateRR.Body.String())
		}

		entry = phase3IUpstreamByID(t, handler, adminToken, "server-a")
		if got, _ := entry["maxConcurrent"].(float64); got != 0 {
			t.Fatalf("unlimited maxConcurrent = %v, want 0", entry["maxConcurrent"])
		}
		if got, _ := entry["assignedUsers"].(float64); got != 1 {
			t.Fatalf("assignedUsers with unlimited maxConcurrent = %v, want 1", entry["assignedUsers"])
		}
	})
}
