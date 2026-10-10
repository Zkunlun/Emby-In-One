package backend

import (
	"fmt"
	"net/http"
	"testing"
)

func TestPhase4DKnownSeriesEpisodePagesAfterFirstSourceWindow(t *testing.T) {
	series := map[string]any{"Id": "series-a", "Type": "Series", "Name": "A Show", "ProductionYear": 2023,
		"ProviderIds": map[string]any{"Tmdb": "show-a"}}
	episodes := []map[string]any{series}
	for i := 0; i < 155; i++ {
		episodes = append(episodes, map[string]any{
			"Id": fmt.Sprintf("ep-a-%03d", i), "Type": "Episode",
			"SeriesId": "series-a", "ParentIndexNumber": 1, "IndexNumber": i + 1,
			"Name": fmt.Sprintf("Episode %03d", i+1),
		})
	}
	withTask6HTTPFixture(t, episodes, nil, func(f *task6HTTPFixture) {
		seriesID := f.app.IDStore.GetOrCreateVirtualID("series-a", "server-a")
		url := "/Shows/" + seriesID + "/Episodes?StartIndex=133&Limit=10"
		page := task6HTTPJSON(t, f.request(t, http.MethodGet, url, nil, ""))
		if got := len(asItems(page)); got != 10 {
			t.Fatalf("known series deep page length %d", got)
		}
		if total, _ := numericInt(page["TotalRecordCount"]); total != 155 {
			t.Fatalf("episode total=%v want 155", page["TotalRecordCount"])
		}
		if f.upstream[0].count("/Shows/series-a/Episodes") < 2 {
			t.Fatal("series source did not continue past first page")
		}
		if f.upstream[1].count("/Shows/") != 0 {
			t.Fatal("known single-source series queried unrelated source")
		}
	})
}
