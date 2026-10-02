package backend

import (
	"fmt"
	"net/http"
	"os"
	"strings"
	"testing"
)

func phase1CCreateAssignedUsers(t *testing.T, handler http.Handler, adminToken string, count int) {
	t.Helper()
	for i := 0; i < count; i++ {
		username := "phase1c-user-" + string(rune('a'+i))
		rr := phase1ACreateUser(t, handler, adminToken, username, []string{"server-a"})
		if rr.Code != http.StatusCreated {
			t.Fatalf("create %s: status=%d body=%s", username, rr.Code, rr.Body.String())
		}
	}
}

func phase1CUpdateMaxConcurrent(t *testing.T, handler http.Handler, adminToken string, value int) int {
	t.Helper()
	rr := doAuthJSON(t, handler, http.MethodPut, "/admin/api/upstream/server-a", map[string]any{
		"maxConcurrent": value,
	}, adminToken)
	return rr.Code
}

func phase1CConfiguredMaxConcurrent(t *testing.T, app *App) int {
	t.Helper()
	cfg := app.ConfigStore.Snapshot()
	for _, upstream := range cfg.Upstream {
		if upstream.ID == "server-a" {
			return upstream.MaxConcurrent
		}
	}
	t.Fatal("server-a missing from config")
	return -1
}

func TestPhase1CMaxConcurrentCapacityContract(t *testing.T) {
	t.Run("cannot lower below assigned user count", func(t *testing.T) {
		upstream := phase1AUpstreamStub(t)
		defer upstream.Close()

		config := phase1AAuthorizationSlotConfig(upstream.URL)
		config = replaceMaxConcurrentForPhase1C(t, config, 3)
		withTempAppConfig(t, config, func(app *App, handler http.Handler) {
			adminToken := loginTokenAs(t, handler, "admin", "secret")
			phase1CCreateAssignedUsers(t, handler, adminToken, 3)

			if got := phase1ACountAssignedUsers(app, "server-a"); got != 3 {
				t.Fatalf("assigned users = %d, want 3 before lowering limit", got)
			}
			configPath := app.ConfigStore.Snapshot().Path
			beforeDisk, err := os.ReadFile(configPath)
			if err != nil {
				t.Fatalf("read config before rejected update: %v", err)
			}
			status := phase1CUpdateMaxConcurrent(t, handler, adminToken, 2)
			if status >= 200 && status < 300 {
				t.Fatalf("lowering maxConcurrent from 3 to 2 unexpectedly succeeded with 3 assigned users")
			}
			if got := phase1CConfiguredMaxConcurrent(t, app); got != 3 {
				t.Fatalf("maxConcurrent changed after rejected update: got %d, want 3", got)
			}
			afterDisk, err := os.ReadFile(configPath)
			if err != nil {
				t.Fatalf("read config after rejected update: %v", err)
			}
			if string(afterDisk) != string(beforeDisk) {
				t.Fatal("config file changed after rejected maxConcurrent update")
			}
		})
	})

	t.Run("raising above assigned user count is allowed", func(t *testing.T) {
		upstream := phase1AUpstreamStub(t)
		defer upstream.Close()

		config := replaceMaxConcurrentForPhase1C(t, phase1AAuthorizationSlotConfig(upstream.URL), 3)
		withTempAppConfig(t, config, func(app *App, handler http.Handler) {
			adminToken := loginTokenAs(t, handler, "admin", "secret")
			phase1CCreateAssignedUsers(t, handler, adminToken, 3)

			status := phase1CUpdateMaxConcurrent(t, handler, adminToken, 5)
			if status != http.StatusOK {
				t.Fatalf("raise maxConcurrent: status=%d, want 200", status)
			}
			if got := phase1CConfiguredMaxConcurrent(t, app); got != 5 {
				t.Fatalf("maxConcurrent = %d after raise, want 5", got)
			}
		})
	})

	t.Run("zero keeps unlimited authorization semantics", func(t *testing.T) {
		upstream := phase1AUpstreamStub(t)
		defer upstream.Close()

		config := replaceMaxConcurrentForPhase1C(t, phase1AAuthorizationSlotConfig(upstream.URL), 3)
		withTempAppConfig(t, config, func(app *App, handler http.Handler) {
			adminToken := loginTokenAs(t, handler, "admin", "secret")
			phase1CCreateAssignedUsers(t, handler, adminToken, 3)

			status := phase1CUpdateMaxConcurrent(t, handler, adminToken, 0)
			if status != http.StatusOK {
				t.Fatalf("set maxConcurrent=0: status=%d, want 200", status)
			}
			if got := phase1CConfiguredMaxConcurrent(t, app); got != 0 {
				t.Fatalf("maxConcurrent = %d after unlimited update, want 0", got)
			}
		})
	})
}

func replaceMaxConcurrentForPhase1C(t *testing.T, config string, value int) string {
	t.Helper()
	old := "    maxConcurrent: 2\n"
	if !strings.Contains(config, old) {
		t.Fatal("phase1A config missing maxConcurrent: 2")
	}
	return strings.Replace(config, old, fmt.Sprintf("    maxConcurrent: %d\n", value), 1)
}
