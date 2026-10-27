package backend

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"sort"
	"testing"
)

func TestShowsEpisodesSameServerSeasonPairs(t *testing.T) {
	calls := []string{}
	failShows := false
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/Users/AuthenticateByName":
			_ = json.NewEncoder(w).Encode(map[string]any{"AccessToken": "upstream-token", "User": map[string]any{"Id": "user-a"}})
		case "/Shows/series-a/Episodes", "/Shows/series-b/Episodes":
			if failShows {
				http.Error(w, "upstream unavailable", http.StatusBadGateway)
				return
			}
			calls = append(calls, r.URL.Path+"?SeasonId="+r.URL.Query().Get("SeasonId"))
			want := "season-a"
			id := "ep-a"
			series := "series-a"
			if r.URL.Path == "/Shows/series-b/Episodes" {
				want, id, series = "season-b", "ep-b", "series-b"
			}
			if r.URL.Query().Get("SeasonId") != want {
				http.Error(w, "mismatched season", http.StatusBadRequest)
				return
			}
			_ = json.NewEncoder(w).Encode(map[string]any{"Items": []map[string]any{
				{"Id": id, "Type": "Episode", "SeriesId": series, "ParentId": want, "ParentIndexNumber": 1, "IndexNumber": 1,
					"Name": "Episode 1", "MediaSources": []any{map[string]any{"Id": id + "-source", "RunTimeTicks": 1000}}},
			}})
		case "/Items", "/Users/user-a/Items":
			parent := r.URL.Query().Get("ParentId")
			if r.URL.Path == "/Items" && parent != "" {
				http.Error(w, "raw Items/ParentId is not supported", http.StatusNotFound)
				return
			}
			if parent == "" {
				_ = json.NewEncoder(w).Encode(map[string]any{"Items": []any{}})
				return
			}
			calls = append(calls, r.URL.Path+"?ParentId="+parent)
			if parent != "season-a" && parent != "season-b" {
				http.Error(w, "unrecognized parent", http.StatusBadRequest)
				return
			}
			series, id := "series-a", "ep-a"
			if parent == "season-b" {
				series, id = "series-b", "ep-b"
			}
			_ = json.NewEncoder(w).Encode(map[string]any{"Items": []map[string]any{
				{"Id": id, "Type": "Episode", "SeriesId": series, "ParentId": parent, "ParentIndexNumber": 1, "IndexNumber": 1,
					"Name": "Episode 1", "MediaSources": []any{map[string]any{"Id": id + "-source", "RunTimeTicks": 1000}}},
			}, "TotalRecordCount": 1, "StartIndex": 0})
		default:
			http.NotFound(w, r)
		}
	}))
	defer upstream.Close()

	config := fmt.Sprintf("server:\n  port: 8096\n  name: Test\n  id: eio-test\nadmin:\n  username: admin\n  password: secret\nplayback:\n  mode: proxy\ntimeouts:\n  api: 30000\n  global: 15000\n  login: 10000\n  healthCheck: 10000\n  healthInterval: 60000\nproxies: []\nupstream:\n  - name: A\n    url: %q\n    username: source-user\n    password: source-password\n", upstream.URL)
	withTempAppConfig(t, config, func(app *App, handler http.Handler) {
		token := loginToken(t, handler, "secret")
		client := app.Upstream.Clients()[0]
		virtualSeries := app.IDStore.GetOrCreateVirtualID("series-a", client.ID)
		app.IDStore.AssociateAdditionalInstance(virtualSeries, "series-b", client.ID)

		parentA := &mergeSeriesEvidence{ServerID: client.ID, SeriesID: "series-a",
			Identity: mergeIdentity{Type: "Series", Name: "Tom and Jerry in New York"}}
		parentB := &mergeSeriesEvidence{ServerID: client.ID, SeriesID: "series-b",
			Identity: mergeIdentity{Type: "Series", Name: "Tom and Jerry in New York"}}
		season := func(id, series string) map[string]any {
			return map[string]any{"Id": id, "Type": "Season", "Name": "Season 1", "SeriesId": series,
				"ParentIndexNumber": 1, "IndexNumber": 1}
		}
		virtualSeason, err := app.IDStore.AssociateMergePair(
			newMergeCandidate(client.ID, season("season-a", "series-a"), parentA, false),
			newMergeCandidate(client.ID, season("season-b", "series-b"), parentB, false), "", "", "")
		if err != nil {
			t.Fatal(err)
		}

		requestURL := "/Shows/" + virtualSeries + "/Episodes?" + url.Values{"SeasonId": {virtualSeason}}.Encode()
		rr := doJSONRequest(t, handler, http.MethodGet, requestURL, nil, token)
		if rr.Code != http.StatusOK {
			t.Fatalf("status %d: %s", rr.Code, rr.Body.String())
		}
		sort.Strings(calls)
		expected := []string{"/Shows/series-a/Episodes?SeasonId=season-a",
			"/Shows/series-b/Episodes?SeasonId=season-b"}
		if len(calls) != len(expected) || calls[0] != expected[0] || calls[1] != expected[1] {
			t.Fatalf("upstream pairings = %v, want %v", calls, expected)
		}
		for _, path := range []string{"/Items", "/Users/user-a/Items"} {
			calls = nil
			rr := doJSONRequest(t, handler, http.MethodGet, path+"?ParentId="+virtualSeason, nil, token)
			if rr.Code != http.StatusOK {
				t.Fatalf("%s returned %d: %s", path, rr.Code, rr.Body.String())
			}
			var data struct {
				Items            []map[string]any `json:"Items"`
				TotalRecordCount int              `json:"TotalRecordCount"`
			}
			if err := json.Unmarshal(rr.Body.Bytes(), &data); err != nil {
				t.Fatal(err)
			}
			if len(data.Items) == 0 || len(data.Items) != data.TotalRecordCount {
				t.Errorf("%s returned inconsistent page: %d items, total=%d", path, len(data.Items), data.TotalRecordCount)
			}
			for _, item := range data.Items {
				if item["ParentId"] != virtualSeason {
					t.Errorf("%s returned wrong virtual parent: %v", path, item["ParentId"])
				}
			}
			sort.Strings(calls)
			userItemsPath := "/Users/user-a/Items"
			if len(calls) != 2 || calls[0] != userItemsPath+"?ParentId=season-a" || calls[1] != userItemsPath+"?ParentId=season-b" {
				t.Errorf("%s requested %v", path, calls)
			}
		}
		// Older persisted singleton Season groups have no Parent.SeriesID.
		// Their one original season must still be sent upstream.
		singleSeason, err := app.IDStore.RegisterMergeItem(newMergeCandidate(client.ID,
			map[string]any{"Id": "season-single", "Type": "Season", "Name": "Season 1", "IndexNumber": 1},
			nil, false), "")
		if err != nil {
			t.Fatal(err)
		}
		if group := app.IDStore.ResolveMergeGroup(singleSeason); group == nil || group.MediaType != "Season" ||
			len(group.Members) != 1 || group.Members[0].Parent != nil {
			t.Fatal("test setup must retain a singleton Season group without parent evidence")
		}
		got := app.seasonIDsForSeriesInstance(nil, singleSeason,
			seriesInstance{OriginalID: "series-a", ServerID: client.ID, Client: client})
		if len(got) != 1 || got[0] != "season-single" {
			t.Errorf("unparented singleton season IDs = %v, want season-single", got)
		}
		if mismatched := app.seasonIDsForSeriesInstance(nil, virtualSeason,
			seriesInstance{OriginalID: "unrelated-series", ServerID: client.ID, Client: client}); len(mismatched) != 0 {
			t.Errorf("known mismatched parent should not fall back: %v", mismatched)
		}
		failShows = true
		rr = doJSONRequest(t, handler, http.MethodGet, requestURL, nil, token)
		if rr.Code != http.StatusBadGateway {
			t.Errorf("all upstream episodes failed: status %d, want 502", rr.Code)
		}
	})
}

func TestMergedItemImageFallbackOn404(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/Users/AuthenticateByName":
			_ = json.NewEncoder(w).Encode(map[string]any{"AccessToken": "source-token", "User": map[string]any{"Id": "user-a"}})
		case "/Items/primary/Images/Primary":
			switch r.URL.Query().Get("ImageFault") {
			case "500":
				http.Error(w, "source image failed", http.StatusInternalServerError)
			case "403":
				http.Error(w, "source access denied", http.StatusForbidden)
			default:
				http.NotFound(w, r)
			}
		case "/Items/secondary/Images/Primary":
			w.Header().Set("Content-Type", "image/png")
			_, _ = w.Write([]byte("secondary-image"))
		default:
			http.NotFound(w, r)
		}
	}))
	defer upstream.Close()
	config := fmt.Sprintf("server:\n  port: 8096\n  name: Test\n  id: eio-test\nadmin:\n  username: admin\n  password: secret\nplayback:\n  mode: proxy\ntimeouts:\n  api: 30000\n  global: 15000\n  login: 10000\n  healthCheck: 10000\n  healthInterval: 60000\nproxies: []\nupstream:\n  - name: A\n    url: %q\n    username: source-user\n    password: source-password\n", upstream.URL)
	withTempAppConfig(t, config, func(app *App, handler http.Handler) {
		token := loginToken(t, handler, "secret")
		client := app.Upstream.Clients()[0]
		virtual := app.IDStore.GetOrCreateVirtualID("primary", client.ID)
		app.IDStore.AssociateAdditionalInstance(virtual, "secondary", client.ID)
		rr := doJSONRequest(t, handler, http.MethodGet, "/Items/"+virtual+"/Images/Primary", nil, token)
		if rr.Code != http.StatusOK || rr.Body.String() != "secondary-image" {
			t.Errorf("image fallback status=%d body=%q", rr.Code, rr.Body.String())
		}
		failed := doJSONRequest(t, handler, http.MethodGet, "/Items/"+virtual+"/Images/Primary?ImageFault=500", nil, token)
		if failed.Code != http.StatusOK || failed.Body.String() != "secondary-image" {
			t.Errorf("image fallback on upstream 500 status=%d body=%q", failed.Code, failed.Body.String())
		}
		forbidden := doJSONRequest(t, handler, http.MethodGet, "/Items/"+virtual+"/Images/Primary?ImageFault=403", nil, token)
		if forbidden.Code != http.StatusForbidden {
			t.Errorf("authentication failure must not fallback: %d", forbidden.Code)
		}
	})
}
