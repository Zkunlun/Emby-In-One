package backend

import (
	"net/http"
	"strings"
	"testing"
)

// Client query casing and root sentinels do not make a source-limited browse.
func TestPhase4BCaseInsensitiveRootAndRandomAvoidWholeLibrary(t *testing.T) {
	aa, bb := make([]map[string]any, 0, 165), make([]map[string]any, 0, 165)
	for i := 0; i < 165; i++ {
		aa = append(aa, phase4Movie("A", i))
		bb = append(bb, phase4Movie("B", i))
	}
	withTask6HTTPFixture(t, aa, bb, func(f *task6HTTPFixture) {
		base := "/Users/" + f.user + "/Items"
		root := f.request(t, http.MethodGet, base+"?pArEnTiD=ROOT&Limit=20", nil, "")
		if root.Code != http.StatusOK || len(asItems(task6HTTPJSON(t, root))) != 20 {
			t.Fatalf("mixed-case root must return global page: %d %s", root.Code, root.Body.String())
		}
		for _, up := range f.upstream {
			if up.count("/Users/user-"+up.label+"/Items") != 1 {
				t.Fatalf("root caused unneeded scanning of %s", up.label)
			}
			up.mu.Lock()
			for _, call := range up.calls {
				if strings.HasSuffix(call.Path, "/Items") {
					for key := range call.Query {
						if strings.EqualFold(key, "ParentId") {
							up.mu.Unlock()
							t.Fatalf("root sentinel leaked to upstream %s: %v", up.label, call.Query)
						}
					}
				}
			}
			up.mu.Unlock()
		}
		random := f.request(t, http.MethodGet, base+"?SortBy=Random&Limit=20", nil, "")
		if random.Code != http.StatusOK || len(asItems(task6HTTPJSON(t, random))) != 20 {
			t.Fatalf("Random page returned %d %s", random.Code, random.Body.String())
		}
		for _, up := range f.upstream {
			if got := up.count("/Users/user-" + up.label + "/Items"); got != 2 {
				t.Fatalf("Random first page made %d total source requests on %s, want 2", got, up.label)
			}
		}
	})
}
