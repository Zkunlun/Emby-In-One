package backend

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

// A source transport failure must not be reported to clients as a genuine
// empty season collection. Hills otherwise treats a transient upstream outage
// as a show with no seasons.
func TestHillsSeasonsDistinguishesFailureFromEmpty(t *testing.T) {
	fail := true
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/Users/AuthenticateByName":
			_ = json.NewEncoder(w).Encode(map[string]any{"AccessToken": "source-token", "User": map[string]any{"Id": "source-user"}})
		case "/Shows/series-a/Seasons":
			if fail {
				http.Error(w, "upstream unavailable", http.StatusServiceUnavailable)
				return
			}
			_ = json.NewEncoder(w).Encode(map[string]any{"Items": []any{}, "TotalRecordCount": 0})
		default:
			http.NotFound(w, r)
		}
	}))
	defer upstream.Close()
	withTempAppConfig(t, singleUpstreamConfig(upstream.URL), func(app *App, handler http.Handler) {
		admin := loginTokenAs(t, handler, "admin", "secret")
		series := app.IDStore.GetOrCreateVirtualID("series-a", app.Upstream.Clients()[0].ID)
		url := "/Shows/" + series + "/Seasons"
		response := doJSONRequest(t, handler, http.MethodGet, url, nil, admin)
		if response.Code != http.StatusBadGateway {
			t.Fatalf("failed upstream was represented as %d, want 502", response.Code)
		}
		fail = false
		response = doJSONRequest(t, handler, http.MethodGet, url, nil, admin)
		if response.Code != http.StatusOK {
			t.Fatalf("upstream's real empty list status=%d", response.Code)
		}
		var data struct {
			Items            []any `json:"Items"`
			TotalRecordCount int   `json:"TotalRecordCount"`
		}
		if err := json.Unmarshal(response.Body.Bytes(), &data); err != nil {
			t.Fatal(err)
		}
		if data.TotalRecordCount != 0 || len(data.Items) != 0 {
			t.Fatalf("expected true empty upstream list, got %s", response.Body.String())
		}
	})
}

func TestHillsSeasonListingSurvivesSlowSecondary(t *testing.T) {
	fast := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/Users/AuthenticateByName":
			_ = json.NewEncoder(w).Encode(map[string]any{"AccessToken": "a", "User": map[string]any{"Id": "ua"}})
		case "/Shows/series-fast/Seasons":
			_ = json.NewEncoder(w).Encode(map[string]any{"Items": []any{map[string]any{"Id": "season-fast", "Type": "Season", "Name": "Season 1", "SeriesId": "series-fast", "IndexNumber": 1}}, "TotalRecordCount": 1})
		default:
			http.NotFound(w, r)
		}
	}))
	defer fast.Close()
	slow := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/Users/AuthenticateByName":
			_ = json.NewEncoder(w).Encode(map[string]any{"AccessToken": "b", "User": map[string]any{"Id": "ub"}})
		case "/Shows/series-slow/Seasons":
			time.Sleep(6 * time.Second)
			_ = json.NewEncoder(w).Encode(map[string]any{"Items": []any{}})
		default:
			http.NotFound(w, r)
		}
	}))
	defer slow.Close()
	config := fmt.Sprintf("server:\n  port: 8096\n  name: Test\n  id: test\nadmin:\n  username: admin\n  password: secret\nplayback:\n  mode: proxy\ntimeouts:\n  api: 12000\n  global: 12000\n  login: 10000\n  healthCheck: 10000\n  metadataGracePeriod: 750\nproxies: []\nupstream:\n  - name: fast\n    url: %q\n    username: u1\n    password: p1\n  - name: slow\n    url: %q\n    username: u2\n    password: p2\n", fast.URL, slow.URL)
	withTempAppConfig(t, config, func(app *App, handler http.Handler) {
		token := loginTokenAs(t, handler, "admin", "secret")
		clients := app.Upstream.Clients()
		id := app.IDStore.GetOrCreateVirtualID("series-fast", clients[0].ID)
		app.IDStore.AssociateAdditionalInstance(id, "series-slow", clients[1].ID)
		start := time.Now()
		rr := doJSONRequest(t, handler, http.MethodGet, "/Shows/"+id+"/Seasons", nil, token)
		elapsed := time.Since(start)
		if rr.Code != http.StatusOK {
			t.Fatalf("status=%d response=%s", rr.Code, rr.Body)
		}
		var obj struct {
			Items []any `json:"Items"`
		}
		if err := json.Unmarshal(rr.Body.Bytes(), &obj); err != nil {
			t.Fatal(err)
		}
		if len(obj.Items) != 1 {
			t.Fatalf("healthy source season suppressed, items=%d", len(obj.Items))
		}
		if elapsed > 4*time.Second {
			t.Fatalf("slow secondary blocked healthy season for %v", elapsed)
		}
	})
}
