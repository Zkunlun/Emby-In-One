package backend

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
)

func TestHillsMediaSourcesHydratesOnlyRealAuthorizedSources(t *testing.T) {
	var detailHasSources atomic.Bool
	detailHasSources.Store(true)
	baseItem := func() map[string]any {
		return map[string]any{
			"Id": "ep-one", "Type": "Episode", "Name": "Episode 1",
			"SeriesId": "series-one", "SeasonId": "season-one",
			"ParentIndexNumber": 1, "IndexNumber": 1,
		}
	}
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodPost && r.URL.Path == "/Users/AuthenticateByName":
			_ = json.NewEncoder(w).Encode(map[string]any{"AccessToken": "token-upstream", "User": map[string]any{"Id": "user-upstream"}})
		case r.Method == http.MethodGet && r.URL.Path == "/Shows/series-one/Episodes":
			_ = json.NewEncoder(w).Encode(map[string]any{"Items": []any{baseItem()}, "TotalRecordCount": 1})
		case r.Method == http.MethodGet && r.URL.Path == "/Items":
			switch r.URL.Query().Get("Ids") {
			case "ep-one":
				item := baseItem()
				if detailHasSources.Load() {
					item["MediaSources"] = []any{map[string]any{"Id": "source-one", "ItemId": "ep-one", "RunTimeTicks": int64(1000)}}
				}
				_ = json.NewEncoder(w).Encode(map[string]any{"Items": []any{item}})
			case "series-one":
				_ = json.NewEncoder(w).Encode(map[string]any{"Items": []any{map[string]any{
					"Id": "series-one", "Type": "Series", "Name": "Series One",
				}}})
			default:
				_ = json.NewEncoder(w).Encode(map[string]any{"Items": []any{}})
			}
		default:
			http.NotFound(w, r)
		}
	}))
	defer upstream.Close()
	withTempAppConfig(t, singleUpstreamConfig(upstream.URL), func(app *App, handler http.Handler) {
		token := createRegularUser(t, handler)
		server := app.Upstream.Clients()[0].ID
		series := app.IDStore.GetOrCreateVirtualID("series-one", server)
		read := func() map[string]any {
			t.Helper()
			rr := doJSONRequest(t, handler, http.MethodGet, "/Shows/"+series+"/Episodes", nil, token)
			if rr.Code != http.StatusOK {
				t.Fatalf("Episodes status=%d body=%s", rr.Code, rr.Body)
			}
			var payload map[string]any
			if err := json.Unmarshal(rr.Body.Bytes(), &payload); err != nil {
				t.Fatal(err)
			}
			items, _ := payload["Items"].([]any)
			if len(items) != 1 {
				t.Fatalf("unexpected item count %d", len(items))
			}
			item, _ := items[0].(map[string]any)
			return item
		}
		item := read()
		sources, ok := item["MediaSources"].([]any)
		if !ok || len(sources) != 1 {
			t.Fatalf("real detail MediaSources not hydrated: %v", item["MediaSources"])
		}
		source, _ := sources[0].(map[string]any)
		virtualSource, _ := source["Id"].(string)
		mapping := app.IDStore.ResolveVirtualID(virtualSource)
		if mapping == nil || mapping.ServerID != server || mapping.OriginalID != "source-one" || source["ItemId"] != item["Id"] {
			t.Fatalf("invalid source authorization/virtual mapping: %#v", source)
		}
		if count, ok := numericInt(item["MediaSourceCount"]); !ok || count != 1 {
			t.Fatalf("MediaSourceCount=%v, want 1", item["MediaSourceCount"])
		}
		detailHasSources.Store(false)
		noSources := read()
		if sources, ok := noSources["MediaSources"].([]any); ok && len(sources) != 0 {
			t.Fatalf("invented sources when upstream list and detail both lack them: %#v", sources)
		}
	})
}
