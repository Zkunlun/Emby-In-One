package backend

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
)

func phase5ResumeMetadataUpstream(t *testing.T, metadataStatus int) *httptest.Server {
	t.Helper()
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodPost && r.URL.Path == "/Users/AuthenticateByName":
			_ = json.NewEncoder(w).Encode(map[string]any{"AccessToken": "fixture-token", "User": map[string]any{"Id": "user-a"}})
		case r.Method == http.MethodGet && r.URL.Path == "/Items":
			if metadataStatus != http.StatusOK {
				http.Error(w, "synthetic metadata outage", metadataStatus)
				return
			}
			_ = json.NewEncoder(w).Encode(map[string]any{"Items": []any{}, "TotalRecordCount": 0})
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(server.Close)
	return server
}

func TestPhase5LocalResumeMetadataOutageIsNotEmptySuccess(t *testing.T) {
	for _, tc := range []struct {
		name       string
		status     int
		wantStatus int
	}{
		{"online upstream returns 500 for metadata", http.StatusInternalServerError, http.StatusBadGateway},
		{"online upstream successfully returns genuinely empty metadata", http.StatusOK, http.StatusOK},
	} {
		t.Run(tc.name, func(t *testing.T) {
			upstream := phase5ResumeMetadataUpstream(t, tc.status)
			withTempAppConfig(t, phase1DPlaybackConfig(upstream.URL, 1), func(app *App, handler http.Handler) {
				token, info, itemID, _ := phase7BSeedSessionState(t, app, handler)
				candidates, err := app.WatchStore.GetResumeItems(info.UserID, 20)
				if err != nil || len(candidates) == 0 {
					t.Fatalf("existing local watch state missing: rows=%d err=%v", len(candidates), err)
				}
				rr := doJSONRequest(t, handler, http.MethodGet, "/Users/"+app.Auth.ProxyUserID()+"/Items/Resume", nil, token)
				if rr.Code != tc.wantStatus {
					t.Fatalf("watch item %s status=%d want=%d body=%s", itemID, rr.Code, tc.wantStatus, rr.Body.String())
				}
			})
		})
	}
}

func TestPhase5LocalNextUpMetadataOutageIsNotEmptySuccess(t *testing.T) {
	for _, tc := range []struct {
		name              string
		status            int
		wantStatus        int
		nextSeasonFailure bool
	}{
		{"online episode lookup fails with HTTP500", http.StatusInternalServerError, http.StatusBadGateway, false},
		{"online episode lookup succeeds but next episode truly absent", http.StatusOK, http.StatusOK, false},
		{"next season lookup fails after current season succeeded", http.StatusOK, http.StatusBadGateway, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				switch {
				case r.Method == http.MethodPost && r.URL.Path == "/Users/AuthenticateByName":
					_ = json.NewEncoder(w).Encode(map[string]any{"AccessToken": "fixture-token", "User": map[string]any{"Id": "user-a"}})
				case r.Method == http.MethodGet && r.URL.Path == "/Shows/series-a/Episodes":
					if tc.nextSeasonFailure && r.URL.Query().Get("Season") == "2" {
						http.Error(w, "synthetic next-season outage", http.StatusInternalServerError)
						return
					}
					if tc.status != http.StatusOK {
						http.Error(w, "synthetic episode API failure", tc.status)
						return
					}
					_ = json.NewEncoder(w).Encode(map[string]any{"Items": []any{}, "TotalRecordCount": 0})
				default:
					http.NotFound(w, r)
				}
			}))
			defer upstream.Close()
			withTempAppConfig(t, phase1DPlaybackConfig(upstream.URL, 1), func(app *App, handler http.Handler) {
				token := phase1ECreateAuthorizedUser(t, handler, []string{"server-a"})
				info := app.Auth.ValidateToken(token)
				if info == nil {
					t.Fatal("regular user not authorized")
				}
				series := app.IDStore.GetOrCreateVirtualID("series-a", "server-a")
				episode := app.IDStore.GetOrCreateVirtualID("episode-a", "server-a")
				if err := app.WatchStore.RecordProgress(&WatchProgress{
					ProxyUserID: info.UserID, VirtualItemID: episode, ServerID: "server-a", OriginalItemID: "episode-a",
					ItemType: "Episode", Name: "Episode 1", SeriesName: "Series A", SeriesVirtualID: series, SeriesOriginalID: "series-a",
					ParentIndexNumber: 1, IndexNumber: 1, PositionTicks: 111, RuntimeTicks: 1000, Played: tc.nextSeasonFailure,
				}); err != nil {
					t.Fatal(err)
				}
				rr := doJSONRequest(t, handler, http.MethodGet, "/Shows/NextUp", nil, token)
				if rr.Code != tc.wantStatus {
					t.Fatalf("next-up with saved watch status=%d want=%d body=%s", rr.Code, tc.wantStatus, rr.Body.String())
				}
			})
		})
	}
}
