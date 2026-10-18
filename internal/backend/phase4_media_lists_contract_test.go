package backend

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"testing"
)

type phase4Call struct {
	Path  string
	Query url.Values
}

func phase4MediaStub(t *testing.T, failMedia bool) (*httptest.Server, *[]phase4Call, *sync.Mutex) {
	t.Helper()
	mu := &sync.Mutex{}
	calls := &[]phase4Call{}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodPost && r.URL.Path == "/Users/AuthenticateByName" {
			_ = json.NewEncoder(w).Encode(map[string]any{"AccessToken": "test-token", "User": map[string]any{"Id": "upstream-user"}})
			return
		}
		if r.Method != http.MethodGet {
			http.NotFound(w, r)
			return
		}
		mu.Lock()
		*calls = append(*calls, phase4Call{Path: r.URL.Path, Query: cloneValues(r.URL.Query())})
		mu.Unlock()
		if failMedia {
			http.Error(w, "synthetic upstream 500", http.StatusInternalServerError)
			return
		}
		item := map[string]any{"Id": "item-1", "ParentId": "library-upstream", "Type": "Movie", "Name": "Fixture Item"}
		switch {
		case strings.HasSuffix(r.URL.Path, "/Items/Latest"):
			_ = json.NewEncoder(w).Encode([]map[string]any{item})
		case strings.HasSuffix(r.URL.Path, "/Items/Resume"), r.URL.Path == "/Users/upstream-user/Items", r.URL.Path == "/Items":
			_ = json.NewEncoder(w).Encode(map[string]any{"Items": []map[string]any{item}, "TotalRecordCount": 1, "StartIndex": 0})
		case r.URL.Path == "/Shows/NextUp":
			_ = json.NewEncoder(w).Encode(map[string]any{"Items": []map[string]any{{"Id": "episode-1", "SeriesId": "series-upstream", "Type": "Episode", "Name": "Episode"}}, "TotalRecordCount": 1})
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(server.Close)
	return server, calls, mu
}

func phase4SnapshotCalls(calls *[]phase4Call, mu *sync.Mutex) []phase4Call {
	mu.Lock()
	defer mu.Unlock()
	return append([]phase4Call(nil), (*calls)...)
}

// A response rewrite may hydrate /Items?Ids after the original list request.
func phase4LastCallForPath(t *testing.T, calls []phase4Call, path string) phase4Call {
	t.Helper()
	for i := len(calls) - 1; i >= 0; i-- {
		if calls[i].Path == path {
			return calls[i]
		}
	}
	t.Fatalf("no upstream request to %s", path)
	return phase4Call{}
}

func TestPhase4LatestParentVariantsAndRootDoNotMisroute(t *testing.T) {
	server, calls, mu := phase4MediaStub(t, false)
	withTempAppConfig(t, singleUpstreamConfig(server.URL), func(app *App, handler http.Handler) {
		token := loginToken(t, handler, "secret")
		parent := app.IDStore.GetOrCreateVirtualID("library-upstream", app.Upstream.Clients()[0].ID)
		for _, key := range []string{"parentId", "parentid"} {
			t.Run(key, func(t *testing.T) {
				path := "/Users/" + app.Auth.ProxyUserID() + "/Items/Latest?" + key + "=" + url.QueryEscape(parent)
				resp := doJSONRequest(t, handler, http.MethodGet, path, nil, token)
				if resp.Code != 200 {
					t.Fatalf("code=%d body=%s", resp.Code, resp.Body.String())
				}
				var rows []map[string]any
				if err := json.Unmarshal(resp.Body.Bytes(), &rows); err != nil || len(rows) != 1 {
					t.Fatalf("items=%v error=%v", rows, err)
				}
				got := phase4SnapshotCalls(calls, mu)
				last := phase4LastCallForPath(t, got, "/Users/upstream-user/Items/Latest")
				if last.Query.Get("ParentId") != "library-upstream" {
					t.Fatalf("wrong parent forwarded: %s", last.Query.Encode())
				}
				if last.Query.Get("parentId") != "" || last.Query.Get("parentid") != "" {
					t.Fatalf("duplicate case variants forwarded: %s", last.Query.Encode())
				}
			})
		}
		for _, q := range []string{"ParentId=0", "parentId=root", "parentid=root"} {
			t.Run(q, func(t *testing.T) {
				resp := doJSONRequest(t, handler, http.MethodGet, "/Users/"+app.Auth.ProxyUserID()+"/Items/Latest?"+q, nil, token)
				if resp.Code != 200 {
					t.Fatalf("code=%d body=%s", resp.Code, resp.Body.String())
				}
				var rows []map[string]any
				if err := json.Unmarshal(resp.Body.Bytes(), &rows); err != nil || len(rows) != 1 {
					t.Fatalf("rows=%v error=%v", rows, err)
				}
				got := phase4SnapshotCalls(calls, mu)
				last := phase4LastCallForPath(t, got, "/Users/upstream-user/Items/Latest")
				if strings.Contains(strings.ToLower(last.Query.Encode()), "parentid") {
					t.Fatalf("root selector leaked upstream: %s", last.Query.Encode())
				}
			})
		}
	})
}

func TestPhase4RootItemsAndResumeDoNotApplyFakeLibraryFilter(t *testing.T) {
	server, calls, mu := phase4MediaStub(t, false)
	withTempAppConfig(t, singleUpstreamConfig(server.URL), func(app *App, handler http.Handler) {
		token := loginToken(t, handler, "secret")
		for _, route := range []string{"/Items?parentid=root", "/Items/Resume?ParentId=0"} {
			path := "/Users/" + app.Auth.ProxyUserID() + route
			rr := doJSONRequest(t, handler, http.MethodGet, path, nil, token)
			if rr.Code != 200 {
				t.Fatalf("path=%s status=%d body=%s", path, rr.Code, rr.Body.String())
			}
			got := phase4SnapshotCalls(calls, mu)
			last := phase4LastCallForPath(t, got, "/Users/upstream-user"+strings.SplitN(route, "?", 2)[0])
			if strings.Contains(strings.ToLower(last.Query.Encode()), "parentid") {
				t.Fatalf("root selector leaked to %s: %s", route, last.Query.Encode())
			}
			var payload map[string]any
			if err := json.Unmarshal(rr.Body.Bytes(), &payload); err != nil {
				t.Fatal(err)
			}
			if len(asItems(payload)) < 1 {
				t.Fatalf("root route %s returned no mock item: %s", path, rr.Body.String())
			}
		}
	})
}

func TestPhase4NextUpSeriesIDCaseVariantRoutesOneSource(t *testing.T) {
	server, calls, mu := phase4MediaStub(t, false)
	withTempAppConfig(t, singleUpstreamConfig(server.URL), func(app *App, handler http.Handler) {
		token := loginToken(t, handler, "secret")
		series := app.IDStore.GetOrCreateVirtualID("series-upstream", app.Upstream.Clients()[0].ID)
		rr := doJSONRequest(t, handler, http.MethodGet, "/Shows/NextUp?seriesId="+url.QueryEscape(series), nil, token)
		if rr.Code != 200 {
			t.Fatalf("code=%d body=%s", rr.Code, rr.Body.String())
		}
		got := phase4SnapshotCalls(calls, mu)
		last := phase4LastCallForPath(t, got, "/Shows/NextUp")
		if last.Query.Get("SeriesId") != "series-upstream" || last.Query.Get("seriesId") != "" {
			t.Fatalf("NextUp query %s", last.Query.Encode())
		}
	})
}

func TestPhase4AllFailedUpstreamsAreNotReportedAsEmptySuccess(t *testing.T) {
	server, _, _ := phase4MediaStub(t, true)
	withTempAppConfig(t, singleUpstreamConfig(server.URL), func(app *App, handler http.Handler) {
		token := loginToken(t, handler, "secret")
		for _, path := range []string{
			"/Users/" + app.Auth.ProxyUserID() + "/Items/Latest",
			"/Users/" + app.Auth.ProxyUserID() + "/Items",
			"/Users/" + app.Auth.ProxyUserID() + "/Items/Resume",
			"/Shows/NextUp",
		} {
			t.Run(path, func(t *testing.T) {
				rr := doJSONRequest(t, handler, http.MethodGet, path, nil, token)
				if rr.Code != http.StatusBadGateway {
					t.Fatalf("all source HTTP 500 should return EIO 502: status=%d body=%s", rr.Code, rr.Body.String())
				}
			})
		}
	})
}

func TestPhase4PartialUpstreamFailureKeepsSuccessfulMediaLists(t *testing.T) {
	good, _, _ := phase4MediaStub(t, false)
	failed, _, _ := phase4MediaStub(t, true)
	config := singleUpstreamConfig(good.URL) + fmt.Sprintf("  - name: \"Broken Fixture\"\n    url: %q\n    username: \"u2\"\n    password: \"p2\"\n", failed.URL)
	withTempAppConfig(t, config, func(app *App, handler http.Handler) {
		token := loginToken(t, handler, "secret")
		paths := []string{"/Users/" + app.Auth.ProxyUserID() + "/Items/Latest", "/Users/" + app.Auth.ProxyUserID() + "/Items", "/Users/" + app.Auth.ProxyUserID() + "/Items/Resume", "/Shows/NextUp"}
		for _, path := range paths {
			t.Run(path, func(t *testing.T) {
				rr := doJSONRequest(t, handler, http.MethodGet, path, nil, token)
				if rr.Code != http.StatusOK {
					t.Fatalf("partial success status=%d body=%s", rr.Code, rr.Body.String())
				}
				var payload any
				if err := json.Unmarshal(rr.Body.Bytes(), &payload); err != nil {
					t.Fatal(err)
				}
				if len(asItems(payload)) == 0 {
					t.Fatalf("successful source items were lost: %s", rr.Body.String())
				}
			})
		}
	})
}

func TestPhase4TrueEmptySuccessfulUpstreamRemains200(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodPost && r.URL.Path == "/Users/AuthenticateByName" {
			_ = json.NewEncoder(w).Encode(map[string]any{"AccessToken": "fixture", "User": map[string]any{"Id": "upstream-user"}})
			return
		}
		if r.Method == http.MethodGet && strings.HasSuffix(r.URL.Path, "/Items/Latest") {
			_ = json.NewEncoder(w).Encode([]any{})
			return
		}
		if r.Method == http.MethodGet {
			_ = json.NewEncoder(w).Encode(map[string]any{"Items": []any{}, "TotalRecordCount": 0, "StartIndex": 0})
			return
		}
		http.NotFound(w, r)
	}))
	t.Cleanup(server.Close)
	withTempAppConfig(t, singleUpstreamConfig(server.URL), func(app *App, handler http.Handler) {
		token := loginToken(t, handler, "secret")
		for _, path := range []string{"/Users/" + app.Auth.ProxyUserID() + "/Items/Latest", "/Users/" + app.Auth.ProxyUserID() + "/Items", "/Users/" + app.Auth.ProxyUserID() + "/Items/Resume", "/Shows/NextUp"} {
			t.Run(path, func(t *testing.T) {
				rr := doJSONRequest(t, handler, http.MethodGet, path, nil, token)
				if rr.Code != http.StatusOK {
					t.Fatalf("valid empty upstream status=%d body=%s", rr.Code, rr.Body.String())
				}
				var payload any
				if err := json.Unmarshal(rr.Body.Bytes(), &payload); err != nil {
					t.Fatal(err)
				}
				if len(asItems(payload)) != 0 {
					t.Fatalf("expected authentic empty response: %s", rr.Body.String())
				}
			})
		}
	})
}

func TestPhase4ScopedResumeAndNextUpFailureIsNotEmpty200(t *testing.T) {
	server, _, _ := phase4MediaStub(t, true)
	withTempAppConfig(t, singleUpstreamConfig(server.URL), func(app *App, handler http.Handler) {
		token := loginToken(t, handler, "secret")
		source := app.Upstream.Clients()[0].ID
		library := app.IDStore.GetOrCreateVirtualID("library-upstream", source)
		series := app.IDStore.GetOrCreateVirtualID("series-upstream", source)
		for _, path := range []string{
			"/Users/" + app.Auth.ProxyUserID() + "/Items/Resume?parentId=" + url.QueryEscape(library),
			"/Shows/NextUp?seriesId=" + url.QueryEscape(series),
		} {
			t.Run(path, func(t *testing.T) {
				rr := doJSONRequest(t, handler, http.MethodGet, path, nil, token)
				if rr.Code != http.StatusBadGateway {
					t.Fatalf("scoped all-fail status=%d body=%s", rr.Code, rr.Body.String())
				}
			})
		}
	})
}
