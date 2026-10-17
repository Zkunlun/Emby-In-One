package backend

import (
	"encoding/json"
	"fmt"
	"net/http"
	"testing"
)

func TestPhase4OverlayAfterPaginationKeepsTotalsAndLocalState(t *testing.T) {
	items := make([]map[string]any, 0, 48)
	for i := 0; i < 48; i++ {
		// The shared upstream account claims watched; the local user is separate.
		items = append(items, filterStubItem(fmt.Sprintf("movie-%03d", i),
			fmt.Sprintf("Title %03d", i), "library", 2025,
			filterStubUserData(false, true, 950)))
	}
	stub := newFilterStub(t, items)
	withTempAppConfig(t, singleUpstreamConfig(stub.server.URL), func(app *App, handler http.Handler) {
		token := createRegularUser(t, handler)
		info := app.Auth.ValidateToken(token)
		if info == nil {
			t.Fatal("no local user")
		}
		serverID := app.Upstream.Clients()[0].ID
		lastID := app.IDStore.GetOrCreateVirtualID("movie-047", serverID)
		if err := app.WatchStore.RecordProgress(&WatchProgress{
			ProxyUserID: info.UserID, VirtualItemID: lastID, ServerID: serverID,
			OriginalItemID: "movie-047", ItemType: "Movie", IsFavorite: true,
			PositionTicks: 300, RuntimeTicks: 1000, Played: true,
		}); err != nil {
			t.Fatal(err)
		}

		for _, q := range []struct {
			url       string
			wantTotal float64
			wantCount int
		}{
			{"/Users/" + app.Auth.ProxyUserID() + "/Items?StartIndex=46&Limit=2", 48, 2},
			{"/Users/" + app.Auth.ProxyUserID() + "/Items?Filters=IsFavorite&StartIndex=0&Limit=1", 1, 1},
		} {
			rr := doJSONRequest(t, handler, http.MethodGet, q.url, nil, token)
			if rr.Code != http.StatusOK {
				t.Fatalf("status=%d body=%s", rr.Code, rr.Body.String())
			}
			var payload map[string]any
			if err := json.Unmarshal(rr.Body.Bytes(), &payload); err != nil {
				t.Fatal(err)
			}
			if payload["TotalRecordCount"] != q.wantTotal {
				t.Fatalf("total=%v want %v", payload["TotalRecordCount"], q.wantTotal)
			}
			rows, _ := payload["Items"].([]any)
			if len(rows) != q.wantCount {
				t.Fatalf("items=%d want %d", len(rows), q.wantCount)
			}
			for _, v := range rows {
				row := v.(map[string]any)
				userdata := row["UserData"].(map[string]any)
				if row["Id"] == lastID {
					if userdata["Played"] != true || userdata["IsFavorite"] != true {
						t.Fatalf("tail item missing local watch: %#v", userdata)
					}
					if ticks, _ := numericInt64(userdata["PlaybackPositionTicks"]); ticks != 300 {
						t.Fatalf("rewatch position=%v", userdata["PlaybackPositionTicks"])
					}
				} else if userdata["Played"] == true {
					t.Fatalf("upstream shared played leaked into local state: %#v", userdata)
				}
			}
		}
	})
}
