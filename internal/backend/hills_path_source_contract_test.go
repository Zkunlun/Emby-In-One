package backend

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync/atomic"
	"testing"
)

func TestHillsRequestedPathHydratesFromSameUpstreamItem(t *testing.T) {
	var episodeDetailCalls atomic.Int32
	var episodeDetailHasPath atomic.Bool
	episodeDetailHasPath.Store(true)
	episode := func() map[string]any {
		return map[string]any{
			"Id": "ep-a", "Type": "Episode", "Name": "Episode 1",
			"SeriesId": "series-a", "SeasonId": "season-a", "ParentIndexNumber": 1, "IndexNumber": 1,
			"MediaSources": []any{map[string]any{"Id": "source-a", "ItemId": "ep-a", "RunTimeTicks": int64(1000)}},
		}
	}
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodPost && r.URL.Path == "/Users/AuthenticateByName":
			_ = json.NewEncoder(w).Encode(map[string]any{"AccessToken": "token-a", "User": map[string]any{"Id": "user-a"}})
		case r.Method == http.MethodGet && r.URL.Path == "/Shows/series-a/Episodes":
			_ = json.NewEncoder(w).Encode(map[string]any{"Items": []any{episode()}, "TotalRecordCount": 1})
		case r.Method == http.MethodGet && r.URL.Path == "/Items":
			id := r.URL.Query().Get("Ids")
			if id == "ep-a" {
				episodeDetailCalls.Add(1)
				item := episode()
				if episodeDetailHasPath.Load() && strings.Contains(r.URL.Query().Get("Fields"), "Path") {
					item["Path"] = "/srv/media/episode-a.mkv"
				}
				_ = json.NewEncoder(w).Encode(map[string]any{"Items": []any{item}})
				return
			}
			if id == "series-a" {
				_ = json.NewEncoder(w).Encode(map[string]any{"Items": []any{map[string]any{
					"Id": "series-a", "Type": "Series", "Name": "Fixture Series", "ProductionYear": 2024,
				}}})
				return
			}
			_ = json.NewEncoder(w).Encode(map[string]any{"Items": []any{}})
		default:
			http.NotFound(w, r)
		}
	}))
	defer upstream.Close()

	withTempAppConfig(t, singleUpstreamConfig(upstream.URL), func(app *App, handler http.Handler) {
		adminToken := loginTokenAs(t, handler, "admin", "secret")
		serverID := app.Upstream.Clients()[0].ID
		virtualSeries := app.IDStore.GetOrCreateVirtualID("series-a", serverID)
		read := func(fields string) map[string]any {
			t.Helper()
			rr := doJSONRequest(t, handler, http.MethodGet,
				"/Shows/"+virtualSeries+"/Episodes?Fields="+url.QueryEscape(fields), nil, adminToken)
			if rr.Code != http.StatusOK {
				t.Fatalf("episodes status=%d body=%s", rr.Code, rr.Body)
			}
			var payload map[string]any
			if err := json.Unmarshal(rr.Body.Bytes(), &payload); err != nil {
				t.Fatal(err)
			}
			items, _ := payload["Items"].([]any)
			if len(items) != 1 {
				t.Fatalf("episodes items=%d", len(items))
			}
			item, _ := items[0].(map[string]any)
			sources, ok := item["MediaSources"].([]any)
			if !ok || len(sources) != 1 {
				t.Fatalf("original source lost: sources=%#v", item["MediaSources"])
			}
			virtualSource, _ := sources[0].(map[string]any)
			resolved := app.IDStore.ResolveVirtualID(virtualSource["Id"].(string))
			if resolved == nil || resolved.OriginalID != "source-a" || virtualSource["ItemId"] != item["Id"] {
				t.Fatalf("media source mapping invalid: %#v", virtualSource)
			}
			return item
		}
		without := read("Overview")
		if _, ok := without["Path"]; ok {
			t.Fatal("Path synthesized even though not requested or present in upstream list")
		}
		if episodeDetailCalls.Load() != 0 {
			t.Fatal("unexpected episode metadata fetch without requested Path")
		}
		withPath := read("Path")
		if withPath["Path"] != "/srv/media/episode-a.mkv" {
			t.Fatalf("requested genuine Path missing, got %v", withPath["Path"])
		}
		if episodeDetailCalls.Load() == 0 {
			t.Fatal("requested Path did not trigger same-item metadata fetch")
		}
		episodeDetailHasPath.Store(false)
		noSourcePath := read("Path")
		if _, found := noSourcePath["Path"]; found {
			t.Fatal("fabricated Path when source item detail has no Path")
		}
	})
}
