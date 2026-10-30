package backend

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"
)

func TestPhase3HydrateIndependentSourcesFetchMetadataConcurrently(t *testing.T) {
	var started, replied atomic.Int32
	var firstReplySawBoth atomic.Bool
	var fetched atomic.Int32
	upstream := func(item, user string) *httptest.Server {
		return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			switch {
			case r.Method == http.MethodPost && r.URL.Path == "/Users/AuthenticateByName":
				_ = json.NewEncoder(w).Encode(map[string]any{"AccessToken": "token-" + item, "User": map[string]any{"Id": user}})
			case r.Method == http.MethodGet && r.URL.Path == "/Items":
				fetched.Add(1)
				started.Add(1)
				time.Sleep(90 * time.Millisecond)
				if replied.Add(1) == 1 {
					firstReplySawBoth.Store(started.Load() == 2)
				}
				_ = json.NewEncoder(w).Encode(map[string]any{"Items": []any{map[string]any{
					"Id": item, "Type": "Movie", "Name": "Movie " + item, "MediaSources": []any{map[string]any{"Id": "ms-" + item, "Container": "mkv"}},
				}}, "TotalRecordCount": 1})
			default:
				http.NotFound(w, r)
			}
		}))
	}
	a := upstream("movie-a", "user-a")
	defer a.Close()
	b := upstream("movie-b", "user-b")
	defer b.Close()
	withTempAppConfig(t, dualUpstreamConfig(a.URL, b.URL), func(app *App, handler http.Handler) {
		clients := app.Upstream.Clients()
		results := []upstreamItemsResult{
			{ServerID: clients[0].ID, Items: []map[string]any{{"Id": "movie-a", "Type": "Movie", "Name": "Movie A"}}},
			{ServerID: clients[1].ID, Items: []map[string]any{{"Id": "movie-b", "Type": "Movie", "Name": "Movie B"}}},
		}
		token := loginToken(t, handler, "secret")
		req := httptest.NewRequest(http.MethodGet, "/Items", nil)
		req = req.WithContext(context.WithValue(req.Context(), requestContextKey{}, &RequestContext{ProxyToken: token, ProxyUser: app.Auth.ValidateToken(token)}))
		got := app.hydrateMergeResults(req, results)
		if fetched.Load() != 2 {
			t.Fatalf("metadata requests=%d want 2", fetched.Load())
		}
		if !firstReplySawBoth.Load() {
			t.Fatal("metadata fetches across independent servers are serialized")
		}
		for i := range got {
			if !got[i].CompleteItems[got[i].Items[0]["Id"].(string)] {
				t.Fatalf("source %d missing verified metadata", i)
			}
			if sources, ok := got[i].Items[0]["MediaSources"].([]any); !ok || len(sources) != 1 {
				t.Fatalf("source %d lost media source: %+v", i, got[i].Items[0])
			}
		}
	})
}

func TestPhase3HydrateCompleteMetadataMakesNoNetworkRequest(t *testing.T) {
	var metadataCalls atomic.Int32
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/Users/AuthenticateByName" {
			_ = json.NewEncoder(w).Encode(map[string]any{"AccessToken": "token-a", "User": map[string]any{"Id": "user-a"}})
			return
		}
		if r.URL.Path == "/Items" {
			metadataCalls.Add(1)
		}
		http.Error(w, "unexpected metadata fetch", 500)
	}))
	defer upstream.Close()
	withTempAppConfig(t, singleUpstreamConfig(upstream.URL), func(app *App, handler http.Handler) {
		results := []upstreamItemsResult{{ServerID: app.Upstream.Clients()[0].ID, Items: []map[string]any{{
			"Id": "movie-a", "Type": "Movie", "MediaSources": []any{map[string]any{"Id": "source-a"}},
		}}}}
		token := loginToken(t, handler, "secret")
		req := httptest.NewRequest(http.MethodGet, "/Items", nil)
		req = req.WithContext(context.WithValue(req.Context(), requestContextKey{}, &RequestContext{ProxyToken: token, ProxyUser: app.Auth.ValidateToken(token)}))
		got := app.hydrateMergeResults(req, results)
		if metadataCalls.Load() != 0 {
			t.Fatalf("complete item made %d redundant queries", metadataCalls.Load())
		}
		if len(got) != 1 {
			t.Fatal("complete item discarded")
		}
	})
}
