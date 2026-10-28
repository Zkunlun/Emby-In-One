package backend

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"
)

// Empty season listings from one provider are not evidence that all instances
// have no seasons. A later non-empty provider must have an opportunity to reply.
func TestHillsShowsEmptyProviderDoesNotHideLaterSeasons(t *testing.T) {
	var secondaryMode atomic.Int32
	first := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/Users/AuthenticateByName":
			_ = json.NewEncoder(w).Encode(map[string]any{"AccessToken": "token-a", "User": map[string]any{"Id": "user-a"}})
		case "/Shows/series-a/Seasons":
			_ = json.NewEncoder(w).Encode(map[string]any{"Items": []any{}, "TotalRecordCount": 0})
		default:
			http.NotFound(w, r)
		}
	}))
	defer first.Close()
	second := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/Users/AuthenticateByName":
			_ = json.NewEncoder(w).Encode(map[string]any{"AccessToken": "token-b", "User": map[string]any{"Id": "user-b"}})
		case "/Shows/series-b/Seasons":
			switch secondaryMode.Load() {
			case 0:
				time.Sleep(250 * time.Millisecond)
				_ = json.NewEncoder(w).Encode(map[string]any{"Items": []any{map[string]any{"Id": "season-b", "Type": "Season", "Name": "Season 1", "SeriesId": "series-b", "IndexNumber": 1}}, "TotalRecordCount": 1})
			case 1:
				http.Error(w, "upstream unavailable", http.StatusBadGateway)
			case 2:
				_ = json.NewEncoder(w).Encode(map[string]any{"Items": []any{}, "TotalRecordCount": 0})
			case 3:
				_ = json.NewEncoder(w).Encode(map[string]any{"NoItemsHere": "invalid list"})
			}
		default:
			http.NotFound(w, r)
		}
	}))
	defer second.Close()
	cfg := fmt.Sprintf("server:\n  port: 8096\n  name: Test\n  id: test\nadmin:\n  username: admin\n  password: secret\nplayback:\n  mode: proxy\ntimeouts:\n  api: 3000\n  global: 3000\n  login: 10000\n  healthCheck: 10000\n  metadataGracePeriod: 50\nproxies: []\nupstream:\n  - name: Empty A\n    url: %q\n    username: u1\n    password: p1\n  - name: Slow B\n    url: %q\n    username: u2\n    password: p2\n", first.URL, second.URL)
	withTempAppConfig(t, cfg, func(app *App, handler http.Handler) {
		token := loginTokenAs(t, handler, "admin", "secret")
		clients := app.Upstream.Clients()
		series := app.IDStore.GetOrCreateVirtualID("series-a", clients[0].ID)
		app.IDStore.AssociateAdditionalInstance(series, "series-b", clients[1].ID)
		query := func() (int, int) {
			t.Helper()
			rr := doJSONRequest(t, handler, http.MethodGet, "/Shows/"+series+"/Seasons", nil, token)
			var data struct {
				Items []any `json:"Items"`
			}
			_ = json.Unmarshal(rr.Body.Bytes(), &data)
			return rr.Code, len(data.Items)
		}
		secondaryMode.Store(0)
		status, n := query()
		if status != http.StatusOK || n != 1 {
			t.Fatalf("fast empty suppressed later nonempty provider: status=%d seasons=%d", status, n)
		}
		secondaryMode.Store(1)
		status, n = query()
		if status != http.StatusBadGateway {
			t.Fatalf("empty success + unavailable source must not claim true empty: status=%d seasons=%d", status, n)
		}
		secondaryMode.Store(2)
		status, n = query()
		if status != http.StatusOK || n != 0 {
			t.Fatalf("all sources genuinely empty: status=%d seasons=%d", status, n)
		}
		secondaryMode.Store(3)
		status, n = query()
		if status != http.StatusBadGateway {
			t.Fatalf("malformed upstream response masked by empty provider: status=%d seasons=%d", status, n)
		}
	})
}
