package backend

import (
	"net/http"
	"strings"
	"testing"
)

// Phase 4A freezes request classification only. These tests intentionally do
// not assert the legacy 5000 cap or any Phase 4B future pagination algorithm.
func TestPhase4AParentBrowseAndLatestRemainSourceScoped(t *testing.T) {
	a := task6HTTPMovie("a-movie", task6HTTPSource("a-version", 1000))
	a["ParentId"] = "library-a"
	b := task6HTTPMovie("b-movie", task6HTTPSource("b-version", 1000))
	b["ParentId"] = "library-b"
	withTask6HTTPFixture(t, []map[string]any{a}, []map[string]any{b}, func(f *task6HTTPFixture) {
		parent := f.app.IDStore.GetOrCreateVirtualID("library-a", "server-a")
		path := "/Users/" + f.user + "/Items?ParentId=" + parent + "&StartIndex=0&Limit=1"
		rows := asItems(task6HTTPJSON(t, f.request(t, http.MethodGet, path, nil, "")))
		if len(rows) != 1 {
			t.Fatalf("source A page rows=%d, want 1", len(rows))
		}
		if f.upstream[0].count("/Users/user-a/Items") == 0 || f.upstream[1].count("/Users/user-b/Items") != 0 {
			t.Fatal("ParentId browse performed cross-source list query")
		}
		if f.app.IDStore.ResolveMergeMember("server-b", "b-movie", "") != "" {
			t.Fatal("ParentId browse discovered unrelated source B item")
		}

		response := f.request(t, http.MethodGet, "/Users/"+f.user+"/Items/Latest?ParentId="+parent+"&Limit=1", nil, "")
		if response.Code != http.StatusOK {
			t.Fatalf("Latest status=%d", response.Code)
		}
		if !strings.HasPrefix(strings.TrimSpace(response.Body.String()), "[") {
			t.Fatalf("Latest response changed from array to envelope: %s", response.Body.String())
		}
		if f.upstream[1].count("/Users/user-b/Items/Latest") != 0 {
			t.Fatal("ParentId Latest queried a different upstream")
		}
	})
}

func TestPhase4AGlobalSearchAndLatestUseBothSources(t *testing.T) {
	a := task6HTTPMovie("a-movie", task6HTTPSource("a-version", 1000))
	b := task6HTTPMovie("b-movie", task6HTTPSource("b-version", 1000))
	withTask6HTTPFixture(t, []map[string]any{a}, []map[string]any{b}, func(f *task6HTTPFixture) {
		url := "/Users/" + f.user + "/Items?SearchTerm=Example&IncludeItemTypes=Movie&Limit=1"
		rr := f.request(t, http.MethodGet, url, nil, "")
		if rr.Code != http.StatusOK {
			t.Fatalf("global search status=%d %s", rr.Code, rr.Body.String())
		}
		if f.upstream[0].count("/Users/user-a/Items") == 0 || f.upstream[1].count("/Users/user-b/Items") == 0 {
			t.Fatal("no-ParentId SearchTerm did not fan out to both allowed sources")
		}
		for _, u := range f.upstream {
			u.mu.Lock()
			found := false
			for _, call := range u.calls {
				if strings.HasSuffix(call.Path, "/Items") && call.Query.Get("SearchTerm") == "Example" {
					found = true
				}
			}
			u.mu.Unlock()
			if !found {
				t.Fatal("SearchTerm was not forwarded to upstream")
			}
		}
		rr = f.request(t, http.MethodGet, "/Users/"+f.user+"/Items/Latest?Limit=1", nil, "")
		if rr.Code != http.StatusOK {
			t.Fatalf("global Latest status=%d", rr.Code)
		}
		if !strings.HasPrefix(strings.TrimSpace(rr.Body.String()), "[") {
			t.Fatalf("Latest must be JSON array, got %s", rr.Body.String())
		}
		if f.upstream[0].count("/Users/user-a/Items/Latest") == 0 || f.upstream[1].count("/Users/user-b/Items/Latest") == 0 {
			t.Fatal("no-ParentId Latest did not fan out to both allowed sources")
		}
	})
}
