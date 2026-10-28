package backend

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
)

func TestHillsSeasonUnplayedUsesCompleteVisibleLocalEpisodes(t *testing.T) {
	var complete atomic.Bool
	var episodeMetadataQueries atomic.Int32
	complete.Store(true)
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodPost && r.URL.Path == "/Users/AuthenticateByName":
			_ = json.NewEncoder(w).Encode(map[string]any{"AccessToken": "tok", "User": map[string]any{"Id": "upstream-user"}})
		case r.Method == http.MethodGet && r.URL.Path == "/Shows/series-a/Seasons":
			_ = json.NewEncoder(w).Encode(map[string]any{"Items": []any{
				map[string]any{"Id": "season-one", "Type": "Season", "Name": "Season 1", "IndexNumber": 1,
					"UserData": map[string]any{"Played": true, "UnplayedItemCount": 99}},
				map[string]any{"Id": "season-two", "Type": "Season", "Name": "Season 2", "IndexNumber": 2,
					"UserData": map[string]any{"Played": true, "UnplayedItemCount": 99}},
			}})
		case r.Method == http.MethodGet && r.URL.Path == "/Shows/series-a/Episodes":
			episodes := []any{
				map[string]any{"Id": "ep-one", "Type": "Episode", "Name": "S1 E1", "SeasonId": "season-one", "SeriesId": "series-a", "ParentIndexNumber": 1, "IndexNumber": 1, "MediaSources": []any{map[string]any{"Id": "ms-one"}}},
				map[string]any{"Id": "ep-two", "Type": "Episode", "Name": "S1 E2", "SeasonId": "season-one", "SeriesId": "series-a", "ParentIndexNumber": 1, "IndexNumber": 2, "MediaSources": []any{map[string]any{"Id": "ms-two"}}},
				map[string]any{"Id": "ep-three", "Type": "Episode", "Name": "S2 E1", "SeasonId": "season-two", "SeriesId": "series-a", "ParentIndexNumber": 2, "IndexNumber": 1, "MediaSources": []any{map[string]any{"Id": "ms-three"}}},
			}
			if !complete.Load() {
				episodes = episodes[:2]
			}
			_ = json.NewEncoder(w).Encode(map[string]any{"Items": episodes, "TotalRecordCount": 3})
		case r.Method == http.MethodGet && r.URL.Path == "/Items":
			if strings.Contains(r.URL.Query().Get("Ids"), "ep-") {
				episodeMetadataQueries.Add(1)
			}
			_ = json.NewEncoder(w).Encode(map[string]any{"Items": []any{}})
		default:
			http.NotFound(w, r)
		}
	}))
	defer upstream.Close()

	withTempAppConfig(t, singleUpstreamConfig(upstream.URL), func(app *App, handler http.Handler) {
		admin := loginTokenAs(t, handler, "admin", "secret")
		user := createRegularUser(t, handler)
		server := app.Upstream.Clients()[0].ID
		series := app.IDStore.GetOrCreateVirtualID("series-a", server)
		seasonOne := app.IDStore.GetOrCreateVirtualID("season-one", server)
		seasonTwo := app.IDStore.GetOrCreateVirtualID("season-two", server)
		played := app.IDStore.GetOrCreateVirtualID("ep-one", server)
		if err := app.WatchStore.RecordProgress(&WatchProgress{
			ProxyUserID: watchUserID(t, app, "child"), VirtualItemID: played,
			ServerID: server, OriginalItemID: "ep-one", ItemType: "Episode",
			SeriesVirtualID: series, ParentIndexNumber: 1, IndexNumber: 1, Played: true,
		}); err != nil {
			t.Fatal(err)
		}
		check := func(token string, expected map[string]int64, shouldExist bool) {
			t.Helper()
			rr := doJSONRequest(t, handler, http.MethodGet, "/Shows/"+series+"/Seasons", nil, token)
			if rr.Code != http.StatusOK {
				t.Fatalf("season response status %d: %s", rr.Code, rr.Body)
			}
			for seasonID, count := range expected {
				ud := itemUserData(t, rr.Body.Bytes(), seasonID)
				got, present := numericInt64(ud["UnplayedItemCount"])
				if present != shouldExist || (shouldExist && got != count) {
					t.Fatalf("season count: got %v (present=%v), want %v (present=%v)", ud["UnplayedItemCount"], present, count, shouldExist)
				}
			}
		}
		check(user, map[string]int64{seasonOne: 1, seasonTwo: 1}, true)
		check(admin, map[string]int64{seasonOne: 99, seasonTwo: 99}, true)
		// A client may request Path on the public Seasons DTO, but the
		// private episode scan used for counts does not need file paths.
		rr := doJSONRequest(t, handler, http.MethodGet, "/Shows/"+series+"/Seasons?Fields=Path", nil, user)
		if rr.Code != http.StatusOK {
			t.Fatalf("path-requested seasons status = %d", rr.Code)
		}
		if got := episodeMetadataQueries.Load(); got != 0 {
			t.Fatalf("unplayed count made %d unnecessary episode Path metadata lookups", got)
		}
		complete.Store(false)
		check(user, map[string]int64{seasonOne: 0, seasonTwo: 0}, false)
	})
}
