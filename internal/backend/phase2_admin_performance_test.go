package backend

import (
	"encoding/json"
	"net/http"
	"sync/atomic"
	"testing"
)

// A user-only lifecycle mutation must not rebuild unrelated media mappings:
// a full IDStore reload is O(all mappings/merge groups) and holds the global
// authorization lifecycle gate. SQLite and IDStore mappings are unchanged here.
func TestPhase2UserUpdateDoesNotReloadUnrelatedIDStore(t *testing.T) {
	withHomeLibraryApp(t, func(app *App, handler http.Handler, _ *atomic.Int32) {
		source := app.Upstream.Clients()[0].ID
		virtual := app.IDStore.GetOrCreateVirtualID("phase2-unrelated-media", source)
		if virtual == "" {
			t.Fatal("fixture mapping missing")
		}
		app.IDStore.mu.RLock()
		entryBefore := app.IDStore.virtualToOriginal[virtual]
		app.IDStore.mu.RUnlock()
		if entryBefore == nil {
			t.Fatal("fixture entry missing")
		}
		token := loginToken(t, handler, "secret")
		create := doJSONRequest(t, handler, http.MethodPost, "/admin/api/users", map[string]any{
			"username": "phase2-user", "password": "password123", "allowedServers": []string{source},
		}, token)
		if create.Code != http.StatusCreated {
			t.Fatalf("create status=%d", create.Code)
		}
		var user struct {
			ID string `json:"id"`
		}
		if err := json.Unmarshal(create.Body.Bytes(), &user); err != nil {
			t.Fatal(err)
		}
		for _, body := range []map[string]any{
			{"enabled": true},              // unchanged permission
			{"allowedServers": []string{}}, // change permission
		} {
			response := doJSONRequest(t, handler, http.MethodPut, "/admin/api/users/"+user.ID, body, token)
			if response.Code != http.StatusOK {
				t.Fatalf("update status=%d body=%s", response.Code, response.Body.String())
			}
			app.IDStore.mu.RLock()
			entryAfter := app.IDStore.virtualToOriginal[virtual]
			app.IDStore.mu.RUnlock()
			if entryAfter != entryBefore {
				t.Fatal("user-only lifecycle rebuilt the unrelated media ID index")
			}
		}
	})
}
