package backend

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"
)

// Complete independent episode scans must overlap. The optional local
// unplayed-count calculation may not turn two source RTTs into their sum.
func TestPhase3LocalSeasonCountsFetchSourcesConcurrently(t *testing.T) {
	var started, replied atomic.Int32
	var firstSawBoth atomic.Bool
	upstream := func(series, season, ep, user string) *httptest.Server {
		return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			switch r.URL.Path {
			case "/Users/AuthenticateByName":
				_ = json.NewEncoder(w).Encode(map[string]any{"AccessToken": "token-" + series, "User": map[string]any{"Id": user}})
			case "/Shows/" + series + "/Seasons":
				_ = json.NewEncoder(w).Encode(map[string]any{"Items": []any{
					map[string]any{"Id": season, "Type": "Season", "IndexNumber": 1, "SeriesId": series, "Name": "Season 1"}}, "TotalRecordCount": 1})
			case "/Shows/" + series + "/Episodes":
				started.Add(1)
				time.Sleep(90 * time.Millisecond)
				if replied.Add(1) == 1 {
					firstSawBoth.Store(started.Load() == 2)
				}
				_ = json.NewEncoder(w).Encode(map[string]any{"Items": []any{
					map[string]any{"Id": ep, "Type": "Episode", "SeasonId": season, "SeriesId": series, "IndexNumber": 1, "ParentIndexNumber": 1, "MediaSources": []any{map[string]any{"Id": "ms-" + ep}}}}, "TotalRecordCount": 1})
			default:
				http.NotFound(w, r)
			}
		}))
	}
	a := upstream("show-a", "season-a", "ep-a", "user-a")
	defer a.Close()
	b := upstream("show-b", "season-b", "ep-b", "user-b")
	defer b.Close()
	withTempAppConfig(t, dualUpstreamConfig(a.URL, b.URL), func(app *App, handler http.Handler) {
		token := createRegularUser(t, handler)
		cs := app.Upstream.Clients()
		show := app.IDStore.GetOrCreateVirtualID("show-a", cs[0].ID)
		app.IDStore.AssociateAdditionalInstance(show, "show-b", cs[1].ID)
		rr := doJSONRequest(t, handler, http.MethodGet, "/Shows/"+show+"/Seasons", nil, token)
		if rr.Code != http.StatusOK {
			t.Fatalf("season status=%d: %s", rr.Code, rr.Body.String())
		}
		if started.Load() != 2 {
			t.Fatalf("episode source requests=%d want 2", started.Load())
		}
		if !firstSawBoth.Load() {
			t.Fatal("local season episode fetches were serial, first returned before other started")
		}
	})
}
