package backend

import (
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"testing"
)

func phase4Movie(server string, index int) map[string]any {
	id := fmt.Sprintf("%s-%04d", server, index)
	movie := task6HTTPMovie(id, task6HTTPSource("v-"+id, 10000))
	movie["Name"] = fmt.Sprintf("Title %s %04d", server, index)
	movie["ProviderIds"] = map[string]any{"Tmdb": fmt.Sprintf("%s-%04d", server, index)}
	return movie
}

func TestPhase4BRootWindowCrossesUpstreamPages(t *testing.T) {
	aa, bb := make([]map[string]any, 0, 165), make([]map[string]any, 0, 165)
	for i := 0; i < 165; i++ {
		aa = append(aa, phase4Movie("A", i))
		bb = append(bb, phase4Movie("B", i))
	}
	withTask6HTTPFixture(t, aa, bb, func(f *task6HTTPFixture) {
		root := "/Users/" + f.user + "/Items?IncludeItemTypes=Movie"
		first := task6HTTPJSON(t, f.request(t, http.MethodGet, root+"&StartIndex=0&Limit=20", nil, ""))
		if len(asItems(first)) != 20 {
			t.Fatalf("first page length %d", len(asItems(first)))
		}
		for _, up := range f.upstream {
			if up.count("/Users/user-"+up.label+"/Items") != 1 {
				t.Fatalf("first page fetched unnecessary additional page from %s", up.label)
			}
			up.mu.Lock()
			var saw bool
			for _, call := range up.calls {
				if strings.HasSuffix(call.Path, "/Items") && call.Query.Get("Limit") == "128" && call.Query.Get("StartIndex") == "0" {
					saw = true
				}
			}
			up.mu.Unlock()
			if !saw {
				t.Fatalf("missing bounded request for %s", up.label)
			}
		}
		second := task6HTTPJSON(t, f.request(t, http.MethodGet, root+"&StartIndex=260&Limit=20", nil, ""))
		if len(asItems(second)) != 20 {
			t.Fatalf("deep page length=%d total=%v", len(asItems(second)), second["TotalRecordCount"])
		}
		if count, _ := numericInt(second["TotalRecordCount"]); count != 330 {
			t.Fatalf("exhausted canonical total must be exact; got %d", count)
		}
		for _, up := range f.upstream {
			if up.count("/Users/user-"+up.label+"/Items") < 3 {
				t.Fatalf("deep page did not walk to second upstream page for %s", up.label)
			}
		}
	})
}

func TestPhase4BCanonicalOverlapStillFillsDeepWindow(t *testing.T) {
	aa, bb := make([]map[string]any, 0, 160), make([]map[string]any, 0, 160)
	for i := 0; i < 160; i++ {
		a, b := phase4Movie("A", i), phase4Movie("B", i)
		name := fmt.Sprintf("Shared Movie %04d", i)
		provider := map[string]any{"Tmdb": fmt.Sprintf("%d", 501000+i)}
		a["Name"], b["Name"] = name, name
		a["ProviderIds"], b["ProviderIds"] = provider, provider
		aa, bb = append(aa, a), append(bb, b)
	}
	withTask6HTTPFixture(t, aa, bb, func(f *task6HTTPFixture) {
		endpoint := "/Users/" + f.user + "/Items?IncludeItemTypes=Movie&StartIndex=120&Limit=30"
		response := f.request(t, http.MethodGet, endpoint, nil, "")
		if response.Code != http.StatusOK {
			t.Fatalf("overlap query status=%d %s", response.Code, response.Body.String())
		}
		result := task6HTTPJSON(t, response)
		if got := len(asItems(result)); got != 30 {
			t.Fatalf("overlap window contains %d, want 30", got)
		}
		total, _ := numericInt(result["TotalRecordCount"])
		if total != 160 {
			t.Fatalf("overlap exhausted cardinality=%d, want 160", total)
		}
		for _, up := range f.upstream {
			if up.count("/Users/user-"+up.label+"/Items") < 2 {
				t.Fatalf("canonical overlap required another page from %s", up.label)
			}
		}
	})
}

func TestPhase4BExplicitGlobalSortUsesCanonicalItemNames(t *testing.T) {
	aa, bb := make([]map[string]any, 0, 40), make([]map[string]any, 0, 40)
	for i := 0; i < 40; i++ {
		a, b := phase4Movie("A", i), phase4Movie("B", i)
		a["Name"] = fmt.Sprintf("Zulu %03d", 39-i)
		b["Name"] = fmt.Sprintf("Alpha %03d", 39-i)
		aa, bb = append(aa, a), append(bb, b)
	}
	withTask6HTTPFixture(t, aa, bb, func(f *task6HTTPFixture) {
		endpoint := "/Users/" + f.user + "/Items?IncludeItemTypes=Movie&SortBy=Name&SortOrder=Ascending&StartIndex=0&Limit=20"
		resp := f.request(t, http.MethodGet, endpoint, nil, "")
		if resp.Code != http.StatusOK {
			t.Fatalf("sort response %d %s", resp.Code, resp.Body.String())
		}
		result := task6HTTPJSON(t, resp)
		rows := asItems(result)
		if len(rows) != 20 {
			t.Fatalf("sorted window len=%d", len(rows))
		}
		for i, row := range rows {
			want := fmt.Sprintf("Alpha %03d", i)
			if row["Name"] != want {
				t.Fatalf("global sorted row %d=%q, want %q", i, row["Name"], want)
			}
		}
		if total, _ := numericInt(result["TotalRecordCount"]); total != 80 {
			t.Fatalf("sorted source exhaustion total=%d", total)
		}
	})
}

func TestPhase4CLatestArrayAndGlobalSearchPaging(t *testing.T) {
	aa, bb := make([]map[string]any, 0, 136), make([]map[string]any, 0, 136)
	for i := 0; i < 136; i++ {
		a, b := phase4Movie("A", i), phase4Movie("B", i)
		a["Name"] = fmt.Sprintf("Needle A %03d", i)
		b["Name"] = fmt.Sprintf("Needle B %03d", i)
		aa = append(aa, a)
		bb = append(bb, b)
	}
	withTask6HTTPFixture(t, aa, bb, func(f *task6HTTPFixture) {
		path := "/Users/" + f.user + "/Items?SearchTerm=Needle&Limit=16&StartIndex=250"
		result := task6HTTPJSON(t, f.request(t, http.MethodGet, path, nil, ""))
		if n := len(asItems(result)); n != 16 {
			t.Fatalf("deep search page len=%d", n)
		}
		response := f.request(t, http.MethodGet, "/Users/"+f.user+"/Items/Latest?Limit=12", nil, "")
		if response.Code != 200 || !strings.HasPrefix(strings.TrimSpace(response.Body.String()), "[") {
			t.Fatalf("global Latest lost array protocol: %d %s", response.Code, response.Body.String())
		}
		var latest []any
		if err := json.Unmarshal(response.Body.Bytes(), &latest); err != nil {
			t.Fatal(err)
		}
		if len(latest) != 12 {
			t.Fatalf("latest returned %d entries, want 12", len(latest))
		}
	})
}
