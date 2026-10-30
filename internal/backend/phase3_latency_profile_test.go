package backend

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"sort"
	"testing"
	"time"
)

func phase3ProfileStat(t *testing.T, label string, values []time.Duration) {
	t.Helper()
	if len(values) != 30 {
		t.Fatalf("%s: expected 30 samples, got %d", label, len(values))
	}
	sort.Slice(values, func(i, j int) bool { return values[i] < values[j] })
	t.Logf("PHASE3_PROFILE %s n=%d p50_ms=%.1f p95_ms=%.1f min_ms=%.1f max_ms=%.1f",
		label, len(values), float64(values[14])/1e6, float64(values[28])/1e6, float64(values[0])/1e6, float64(values[29])/1e6)
}

// The baseline and candidate use the exact same synthetic RTTs and DTOs.
// This is a transport-processing comparison, not a production-client KPI.
func TestPhase3MetadataLatencyProfile(t *testing.T) {
	makeUpstream := func(id, user string, delay time.Duration) *httptest.Server {
		return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			switch {
			case r.Method == http.MethodPost && r.URL.Path == "/Users/AuthenticateByName":
				_ = json.NewEncoder(w).Encode(map[string]any{"AccessToken": "token-" + id, "User": map[string]any{"Id": user}})
			case r.Method == http.MethodGet && r.URL.Path == "/Items":
				time.Sleep(delay)
				_ = json.NewEncoder(w).Encode(map[string]any{"Items": []any{map[string]any{"Id": id, "Type": "Movie", "Name": "Movie " + id,
					"MediaSources": []any{map[string]any{"Id": "source-" + id}}}}, "TotalRecordCount": 1})
			default:
				http.NotFound(w, r)
			}
		}))
	}
	a := makeUpstream("movie-a", "user-a", 60*time.Millisecond)
	defer a.Close()
	b := makeUpstream("movie-b", "user-b", 110*time.Millisecond)
	defer b.Close()
	withTempAppConfig(t, dualUpstreamConfig(a.URL, b.URL), func(app *App, handler http.Handler) {
		token := loginToken(t, handler, "secret")
		clients := app.Upstream.Clients()
		ctx := context.WithValue(context.Background(), requestContextKey{}, &RequestContext{
			ProxyToken: token, ProxyUser: app.Auth.ValidateToken(token)})
		samples := make([]time.Duration, 0, 30)
		for n := 0; n < 31; n++ {
			results := []upstreamItemsResult{
				{ServerID: clients[0].ID, Items: []map[string]any{{"Id": "movie-a", "Type": "Movie", "Name": "Movie A"}}},
				{ServerID: clients[1].ID, Items: []map[string]any{{"Id": "movie-b", "Type": "Movie", "Name": "Movie B"}}},
			}
			start := time.Now()
			got := app.hydrateMergeResults(httptest.NewRequest(http.MethodGet, "/Items", nil).WithContext(ctx), results)
			dur := time.Since(start)
			for i := range got {
				if len(asItems(map[string]any{"Items": got[i].Items[0]["MediaSources"]})) != 1 || !got[i].CompleteItems[got[i].Items[0]["Id"].(string)] {
					t.Fatalf("sample %d source %d incomplete", n, i)
				}
			}
			if n > 0 {
				samples = append(samples, dur)
			}
		}
		phase3ProfileStat(t, "metadata-hydration", samples)
	})
}

// User-visible /Shows/{id}/Seasons handler, including its best-effort local
// unplayed count, is timed end-to-end with two independently delayed sources.
func TestPhase3SeasonCountsLatencyProfile(t *testing.T) {
	makeUpstream := func(series, season, episode, user string, delay time.Duration) *httptest.Server {
		return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			switch r.URL.Path {
			case "/Users/AuthenticateByName":
				_ = json.NewEncoder(w).Encode(map[string]any{"AccessToken": "token-" + series, "User": map[string]any{"Id": user}})
			case "/Shows/" + series + "/Seasons":
				_ = json.NewEncoder(w).Encode(map[string]any{"Items": []any{map[string]any{
					"Id": season, "Type": "Season", "Name": "Season 1", "IndexNumber": 1, "SeriesId": series}},
					"TotalRecordCount": 1})
			case "/Shows/" + series + "/Episodes":
				time.Sleep(delay)
				_ = json.NewEncoder(w).Encode(map[string]any{"Items": []any{map[string]any{
					"Id": episode, "Type": "Episode", "SeasonId": season, "SeriesId": series,
					"IndexNumber": 1, "ParentIndexNumber": 1,
					"MediaSources": []any{map[string]any{"Id": "ms-" + episode}}}}, "TotalRecordCount": 1})
			default:
				http.NotFound(w, r)
			}
		}))
	}
	a := makeUpstream("show-a", "season-a", "ep-a", "user-a", 60*time.Millisecond)
	defer a.Close()
	b := makeUpstream("show-b", "season-b", "ep-b", "user-b", 110*time.Millisecond)
	defer b.Close()
	withTempAppConfig(t, dualUpstreamConfig(a.URL, b.URL), func(app *App, handler http.Handler) {
		token := createRegularUser(t, handler)
		clients := app.Upstream.Clients()
		item := app.IDStore.GetOrCreateVirtualID("show-a", clients[0].ID)
		app.IDStore.AssociateAdditionalInstance(item, "show-b", clients[1].ID)
		samples := make([]time.Duration, 0, 30)
		for i := 0; i < 31; i++ {
			start := time.Now()
			rr := doJSONRequest(t, handler, http.MethodGet, "/Shows/"+item+"/Seasons", nil, token)
			dur := time.Since(start)
			if rr.Code != http.StatusOK {
				t.Fatalf("seasons sample %d HTTP %d %s", i, rr.Code, rr.Body.String())
			}
			var body struct{ Items []any }
			if err := json.Unmarshal(rr.Body.Bytes(), &body); err != nil || len(body.Items) == 0 {
				t.Fatalf("sample %d lost seasons: %v %s", i, err, rr.Body.String())
			}
			if i > 0 {
				samples = append(samples, dur)
			}
		}
		phase3ProfileStat(t, "seasons-local-counts", samples)
	})
}
