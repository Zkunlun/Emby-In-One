package backend

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
)

// This integration suite follows SenPlayer/1.0.0's redacted upstream capture:
// login, Counts, Views, Resume, Items, NextUp. All upstream data is synthetic.
func phase4SenPlayerUpstream(t *testing.T, unavailable *atomic.Bool) (*httptest.Server, *atomic.Int32) {
	t.Helper()
	countCalls := &atomic.Int32{}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/Users/AuthenticateByName" && r.Method == http.MethodPost {
			_ = json.NewEncoder(w).Encode(map[string]any{
				"AccessToken": "synthetic-upstream-token",
				"User":        map[string]any{"Id": "fixture-upstream-user"},
			})
			return
		}
		if r.URL.Path == "/System/Info/Public" {
			_ = json.NewEncoder(w).Encode(map[string]any{"Id": "fixture-server", "ServerName": "synthetic"})
			return
		}
		if r.URL.Path == "/System/Ext/ServerDomains" {
			http.NotFound(w, r)
			return
		}
		if r.URL.Path == "/Items/Counts" {
			countCalls.Add(1)
			if unavailable != nil && unavailable.Load() {
				http.Error(w, "synthetic counts temporarily unavailable", http.StatusServiceUnavailable)
				return
			}
			fields := map[string]int{"MovieCount": 4, "SeriesCount": 2, "EpisodeCount": 9}
			for _, name := range officialOptionalCounts {
				fields[name] = 1
			}
			_ = json.NewEncoder(w).Encode(fields)
			return
		}
		if strings.HasSuffix(r.URL.Path, "/Views") && r.Method == http.MethodGet {
			if r.URL.Query().Get("UserId") != "fixture-upstream-user" {
				http.Error(w, "wrong upstream identity", http.StatusBadRequest)
				return
			}
			_ = json.NewEncoder(w).Encode(map[string]any{
				"Items":            []map[string]any{{"Id": "library-1", "Name": "Movies", "Type": "CollectionFolder", "CollectionType": "movies", "IsFolder": true}},
				"TotalRecordCount": 1,
			})
			return
		}
		movie := map[string]any{"Id": "movie-1", "ParentId": "library-1", "Name": "Fixture Movie", "Type": "Movie", "IsFolder": false}
		if r.Method == http.MethodGet && strings.HasSuffix(r.URL.Path, "/Items") {
			_ = json.NewEncoder(w).Encode(map[string]any{"Items": []map[string]any{movie}, "TotalRecordCount": 1, "StartIndex": 0})
			return
		}
		if r.Method == http.MethodGet && strings.HasSuffix(r.URL.Path, "/Items/Resume") {
			_ = json.NewEncoder(w).Encode(map[string]any{"Items": []map[string]any{movie}, "TotalRecordCount": 1})
			return
		}
		if r.Method == http.MethodGet && r.URL.Path == "/Shows/NextUp" {
			_ = json.NewEncoder(w).Encode(map[string]any{
				"Items":            []map[string]any{{"Id": "episode-1", "Name": "Fixture Episode", "SeriesId": "series-1", "Type": "Episode"}},
				"TotalRecordCount": 1,
			})
			return
		}
		http.NotFound(w, r)
	}))
	t.Cleanup(server.Close)
	return server, countCalls
}

func phase4SenPlayerRequest(handler http.Handler, method, path, token string, body []byte) *httptest.ResponseRecorder {
	req := httptest.NewRequest(method, path, bytes.NewReader(body))
	req.Header.Set("User-Agent", "SenPlayer/1.0.0")
	req.Header.Set("X-Emby-Client", "SenPlayer")
	req.Header.Set("X-Emby-Device-Id", "phase4-synthetic-device")
	if len(body) > 0 {
		req.Header.Set("Content-Type", "application/json")
	}
	if token != "" {
		req.Header.Set("X-Emby-Token", token)
	}
	rr := httptest.NewRecorder()
	handler.ServeHTTP(rr, req)
	return rr
}

func phase4SenPlayerLogin(t *testing.T, handler http.Handler) string {
	t.Helper()
	body := []byte("{\"Username\":\"admin\",\"Pw\":\"secret\"}")
	rr := phase4SenPlayerRequest(handler, http.MethodPost, "/emby/Users/AuthenticateByName", "", body)
	if rr.Code != http.StatusOK {
		t.Fatalf("SenPlayer login status=%d body=%s", rr.Code, rr.Body.String())
	}
	var payload struct{ AccessToken string }
	if err := json.Unmarshal(rr.Body.Bytes(), &payload); err != nil || payload.AccessToken == "" {
		t.Fatalf("SenPlayer auth invalid: %v", err)
	}
	return payload.AccessToken
}

func phase4SenPlayerItems(t *testing.T, rr *httptest.ResponseRecorder) ([]map[string]any, int) {
	t.Helper()
	var payload struct {
		Items            []map[string]any
		TotalRecordCount int
	}
	if err := json.Unmarshal(rr.Body.Bytes(), &payload); err != nil {
		t.Fatalf("invalid Items JSON: %v body=%s", err, rr.Body.String())
	}
	return payload.Items, payload.TotalRecordCount
}

func TestPhase4SenPlayerFullUpstreamSequenceOnEIO(t *testing.T) {
	source, countCalls := phase4SenPlayerUpstream(t, nil)
	withTempAppConfig(t, singleUpstreamConfig(source.URL), func(app *App, handler http.Handler) {
		token := phase4SenPlayerLogin(t, handler)
		uid := app.Auth.ProxyUserID()
		id := app.Upstream.Clients()[0].ID
		countsTestEventually(t, func() bool { return app.mediaCounts.cache.read(id).HasSnapshot })

		publicInfo := phase4SenPlayerRequest(handler, http.MethodGet, "/emby/System/Info/Public", "", nil)
		if publicInfo.Code != http.StatusOK {
			t.Fatalf("public system info status=%d", publicInfo.Code)
		}
		// The real source returned 404 here; SenPlayer continued loading.
		domains := phase4SenPlayerRequest(handler, http.MethodGet, "/emby/System/Ext/ServerDomains", token, nil)
		if domains.Code != http.StatusNotFound && domains.Code != http.StatusBadRequest {
			t.Fatalf("unexpected ServerDomains status=%d", domains.Code)
		}

		counts := phase4SenPlayerRequest(handler, http.MethodGet, "/emby/Items/Counts", token, nil)
		if counts.Code != http.StatusOK {
			t.Fatalf("Counts status=%d body=%s", counts.Code, counts.Body.String())
		}
		var countsFields map[string]int64
		if err := json.Unmarshal(counts.Body.Bytes(), &countsFields); err != nil || len(countsFields) != 14 ||
			countsFields["MovieCount"] != 4 || countsFields["EpisodeCount"] != 9 {
			t.Fatalf("Counts 14-field contract: fields=%v err=%v", countsFields, err)
		}

		views := phase4SenPlayerRequest(handler, http.MethodGet,
			"/emby/Users/"+uid+"/Views?EnableImageTypes=Primary,Backdrop&Fields=PrimaryImageAspectRatio&IncludeExternalContent=false&IncludeHidden=false&Limit=20&Recursive=true&StartIndex=0", token, nil)
		if views.Code != http.StatusOK {
			t.Fatalf("Views status=%d body=%s", views.Code, views.Body.String())
		}
		libraries, total := phase4SenPlayerItems(t, views)
		if len(libraries) != 1 || total != 1 {
			t.Fatalf("Views not usable: %v / %d", libraries, total)
		}
		virtualID, ok := libraries[0]["Id"].(string)
		if !ok || virtualID == "" || virtualID == "library-1" {
			t.Fatalf("virtual library ID missing: %v", libraries[0])
		}

		resume := phase4SenPlayerRequest(handler, http.MethodGet,
			"/emby/Users/"+uid+"/Items/Resume?EnableImageTypes=Primary,Backdrop&Fields=PrimaryImageAspectRatio&Limit=20&MediaTypes=Video&Recursive=true&StartIndex=0", token, nil)
		if resume.Code != http.StatusOK {
			t.Fatalf("Resume status=%d body=%s", resume.Code, resume.Body.String())
		}
		_, _ = phase4SenPlayerItems(t, resume)

		items := phase4SenPlayerRequest(handler, http.MethodGet,
			"/emby/Users/"+uid+"/Items?EnableImageTypes=Primary,Backdrop&Fields=PrimaryImageAspectRatio&IncludeItemTypes=Movie&Limit=20&ParentId="+virtualID+"&Recursive=true&SortBy=SortName&SortOrder=Ascending&StartIndex=0", token, nil)
		if items.Code != http.StatusOK {
			t.Fatalf("Items status=%d body=%s", items.Code, items.Body.String())
		}
		movies, recordCount := phase4SenPlayerItems(t, items)
		if len(movies) != 1 || recordCount != 1 {
			t.Fatalf("expected movie: movies=%v total=%d", movies, recordCount)
		}

		next := phase4SenPlayerRequest(handler, http.MethodGet,
			"/emby/Shows/NextUp?EnableImageTypes=Primary&Fields=PrimaryImageAspectRatio&Limit=20&Recursive=true&SeriesId=series-1&StartIndex=0&UserId="+uid, token, nil)
		if next.Code != http.StatusOK {
			t.Fatalf("NextUp status=%d body=%s", next.Code, next.Body.String())
		}
		_, _ = phase4SenPlayerItems(t, next)
		if countCalls.Load() != 1 {
			t.Fatalf("client requests unexpectedly recollected upstream Counts: %d", countCalls.Load())
		}
	})
}

func TestPhase4SenPlayerCountsFailureDoesNotSilentlyMakeViewsEmpty(t *testing.T) {
	var unavailable atomic.Bool
	unavailable.Store(true)
	source, _ := phase4SenPlayerUpstream(t, &unavailable)
	withTempAppConfig(t, singleUpstreamConfig(source.URL), func(app *App, handler http.Handler) {
		token := phase4SenPlayerLogin(t, handler)
		uid := app.Auth.ProxyUserID()
		id := app.Upstream.Clients()[0].ID
		countsTestEventually(t, func() bool {
			view := app.mediaCounts.cache.read(id)
			return view.Pending && !view.InFlight && view.LastErrorClass != ""
		})
		counts := phase4SenPlayerRequest(handler, http.MethodGet, "/emby/Items/Counts", token, nil)
		if counts.Code != http.StatusServiceUnavailable {
			t.Fatalf("unknown core Counts must remain 503: %d", counts.Code)
		}
		views := phase4SenPlayerRequest(handler, http.MethodGet, "/emby/Users/"+uid+"/Views?Limit=20&StartIndex=0", token, nil)
		if views.Code != http.StatusOK {
			t.Fatalf("Views blocked by Counts error: status=%d", views.Code)
		}
		libraries, total := phase4SenPlayerItems(t, views)
		if len(libraries) != 1 || total != 1 {
			t.Fatalf("Counts error suppressed Views: %v / %d", libraries, total)
		}
	})
}

func TestPhase4SenPlayerPartialCountsFailureDoesNotSuppressTwoLibraries(t *testing.T) {
	good, _ := phase4SenPlayerUpstream(t, nil)
	var unavailable atomic.Bool
	unavailable.Store(true)
	bad, _ := phase4SenPlayerUpstream(t, &unavailable)
	withTempAppConfig(t, dualUpstreamConfig(good.URL, bad.URL), func(app *App, handler http.Handler) {
		token := phase4SenPlayerLogin(t, handler)
		uid := app.Auth.ProxyUserID()
		clients := app.Upstream.Clients()
		countsTestEventually(t, func() bool {
			goodState := app.mediaCounts.cache.read(clients[0].ID)
			badState := app.mediaCounts.cache.read(clients[1].ID)
			return goodState.HasSnapshot && badState.Pending && !badState.InFlight &&
				badState.LastErrorClass != ""
		})

		counts := phase4SenPlayerRequest(handler, http.MethodGet, "/emby/Items/Counts", token, nil)
		if counts.Code != http.StatusServiceUnavailable {
			t.Fatalf("Counts must not fabricate a complete aggregate when B fails: status=%d body=%s", counts.Code, counts.Body.String())
		}
		views := phase4SenPlayerRequest(handler, http.MethodGet,
			"/emby/Users/"+uid+"/Views?Fields=PrimaryImageAspectRatio&Limit=100&Recursive=true&StartIndex=0", token, nil)
		if views.Code != http.StatusOK {
			t.Fatalf("Views should remain available: status=%d body=%s", views.Code, views.Body.String())
		}
		libraries, total := phase4SenPlayerItems(t, views)
		if total != 2 || len(libraries) != 2 {
			t.Fatalf("two usable libraries expected despite Counts 503: total=%d libraries=%v", total, libraries)
		}
		first, _ := libraries[0]["Id"].(string)
		second, _ := libraries[1]["Id"].(string)
		if first == "" || second == "" || first == second {
			t.Fatalf("library virtual IDs not isolated across sources: %v", libraries)
		}
	})
}

func TestPhase4SenPlayerAnonymousRequestsCannotReadCountsViewsOrItems(t *testing.T) {
	source, _ := phase4SenPlayerUpstream(t, nil)
	withTempAppConfig(t, singleUpstreamConfig(source.URL), func(app *App, handler http.Handler) {
		uid := app.Auth.ProxyUserID()
		for _, path := range []string{
			"/emby/Items/Counts",
			"/emby/Users/" + uid + "/Views",
			"/emby/Users/" + uid + "/Items?Limit=20&StartIndex=0",
		} {
			rr := phase4SenPlayerRequest(handler, http.MethodGet, path, "", nil)
			if rr.Code != http.StatusUnauthorized {
				t.Fatalf("unauthenticated %s returned %d instead of 401", path, rr.Code)
			}
		}
	})
}
