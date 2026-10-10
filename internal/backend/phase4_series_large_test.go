package backend

import (
	"fmt"
	"net/http"
	"testing"
)

// A single already mapped series is a bounded passive depth query, not a
// request to enumerate the other upstream servers' television libraries.
func TestPhase4DKnownSeriesEpisodeTailBeyondFiveThousand(t *testing.T) {
	if testing.Short() {
		t.Skip("large series fixture")
	}
	const episodeCount = 5205
	series := map[string]any{
		"Id": "long-series-a", "Type": "Series",
		"Name": "Long Series", "ProductionYear": 2024,
		"ProviderIds": map[string]any{"Tmdb": "9998123"},
	}
	items := make([]map[string]any, 0, episodeCount+1)
	items = append(items, series)
	for i := 0; i < episodeCount; i++ {
		id := fmt.Sprintf("long-episode-%05d", i)
		items = append(items, map[string]any{
			"Id": id, "Type": "Episode", "Name": fmt.Sprintf("Episode %05d", i+1),
			"SeriesId": "long-series-a", "ParentIndexNumber": 1, "IndexNumber": i + 1,
			"MediaSources": []any{map[string]any{"Id": "source-" + id, "RunTimeTicks": 1000}},
		})
	}
	withTask6HTTPFixture(t, items, nil, func(f *task6HTTPFixture) {
		seriesID := f.app.IDStore.GetOrCreateVirtualID("long-series-a", "server-a")
		path := "/Shows/" + seriesID + "/Episodes"
		response := f.request(t, http.MethodGet, path+"?StartIndex=5010&Limit=12", nil, "")
		if response.Code != http.StatusOK {
			t.Fatalf("deep series status %d: %s", response.Code, response.Body.String())
		}
		result := task6HTTPJSON(t, response)
		if got := len(asItems(result)); got != 12 {
			t.Fatalf("deep episode window %d, want 12", got)
		}
		if provisional, valid := numericInt(result["TotalRecordCount"]); !valid || provisional <= 5022 {
			t.Fatalf("deep series was prematurely exhausted: %v", result["TotalRecordCount"])
		}
		last := f.request(t, http.MethodGet, path+"?StartIndex=5195&Limit=10", nil, "")
		if last.Code != http.StatusOK {
			t.Fatalf("last series status %d: %s", last.Code, last.Body.String())
		}
		final := task6HTTPJSON(t, last)
		if got := len(asItems(final)); got != 10 {
			t.Fatalf("last episode page length %d", got)
		}
		if exact, _ := numericInt(final["TotalRecordCount"]); exact != episodeCount {
			t.Fatalf("exhausted episode total=%v want %d", final["TotalRecordCount"], episodeCount)
		}
		if n := f.upstream[0].count("/Shows/long-series-a/Episodes"); n < 20 {
			t.Fatalf("known series deep-page queries did not cross the old cap: %d", n)
		}
		if n := f.upstream[1].count("/Shows/"); n != 0 {
			t.Fatalf("known single-source series fetched unrelated server B: %d", n)
		}
	})
}
