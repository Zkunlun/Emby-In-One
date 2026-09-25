package backend

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync/atomic"
	"testing"
)

func hasLocalEmbyTokenQuery(query url.Values) bool {
	for key := range query {
		if strings.EqualFold(key, "X-Emby-Token") {
			return true
		}
	}
	return false
}

func TestLocalTokenQueryCapyPlaybackInfo(t *testing.T) {
	var sawLocalQueryToken, sawExpectedUserID, sawExpectedAuthHeader, sawIsPlayback atomic.Bool
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodPost && r.URL.Path == "/Users/AuthenticateByName" {
			_ = json.NewEncoder(w).Encode(map[string]any{"AccessToken": "upstream-token", "User": map[string]any{"Id": "user-a"}})
			return
		}
		if r.Method != http.MethodPost || r.URL.Path != "/Items/movie-a/PlaybackInfo" {
			http.NotFound(w, r)
			return
		}
		query := r.URL.Query()
		sawLocalQueryToken.Store(hasLocalEmbyTokenQuery(query))
		sawExpectedUserID.Store(query.Get("UserId") == "user-a")
		sawExpectedAuthHeader.Store(r.Header.Get("X-Emby-Token") == "upstream-token")
		sawIsPlayback.Store(query.Get("IsPlayback") == "true")
		if hasLocalEmbyTokenQuery(query) {
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		var body map[string]any
		_ = json.NewDecoder(r.Body).Decode(&body)
		if body["UserId"] != "user-a" {
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"MediaSources": []map[string]any{{"Id": "ms-a", "Container": "mp4"}}})
	}))
	defer upstream.Close()

	withTempAppConfig(t, singleUpstreamConfig(upstream.URL), func(app *App, handler http.Handler) {
		token := loginToken(t, handler, "secret")
		virtualItem := app.IDStore.GetOrCreateVirtualID("movie-a", app.Upstream.Clients()[0].ID)
		query := url.Values{
			"UserId":                {app.Auth.ProxyUserID()},
			"X-Emby-Token":          {token},
			"X-Emby-Client":         {"CapyPlayer"},
			"X-Emby-Client-Version": {"1.1.6"},
			"X-Emby-Device-Id":      {"capy-device"},
			"IsPlayback":            {"true"},
		}
		rr := doJSONRequest(t, handler, http.MethodPost, "/Items/"+virtualItem+"/PlaybackInfo?"+query.Encode(),
			map[string]any{"DeviceProfile": map[string]any{}}, token)
		if rr.Code != http.StatusOK {
			t.Errorf("Capy-shaped PlaybackInfo status = %d, want 200", rr.Code)
		}
		if sawLocalQueryToken.Load() {
			t.Error("strict upstream saw local X-Emby-Token query")
		}
		if !sawExpectedUserID.Load() || !sawExpectedAuthHeader.Load() || !sawIsPlayback.Load() {
			t.Errorf("upstream user/auth/IsPlayback changed: user=%v auth=%v playback=%v", sawExpectedUserID.Load(), sawExpectedAuthHeader.Load(), sawIsPlayback.Load())
		}
	})
}

func TestLocalTokenQueryHillsDiscovery(t *testing.T) {
	type upstreamProbe struct {
		server           *httptest.Server
		queryTokenSeen   atomic.Bool
		searchTermSeen   atomic.Bool
		includeTypesSeen atomic.Bool
		recursiveSeen    atomic.Bool
	}
	newUpstream := func(userID, label string, strict bool) *upstreamProbe {
		probe := &upstreamProbe{}
		probe.server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if r.Method == http.MethodPost && r.URL.Path == "/Users/AuthenticateByName" {
				_ = json.NewEncoder(w).Encode(map[string]any{"AccessToken": "upstream-" + label, "User": map[string]any{"Id": userID}})
				return
			}
			if hasLocalEmbyTokenQuery(r.URL.Query()) {
				probe.queryTokenSeen.Store(true)
				if strict {
					w.WriteHeader(http.StatusBadRequest)
					return
				}
			}
			switch r.URL.Path {
			case "/Users/" + userID + "/Views":
				_ = json.NewEncoder(w).Encode(map[string]any{"Items": []map[string]any{{"Id": label + "-view", "Name": label + " Library", "Type": "UserView"}}})
			case "/Users/" + userID + "/Items":
				query := r.URL.Query()
				probe.searchTermSeen.Store(query.Get("SearchTerm") == "movie")
				probe.includeTypesSeen.Store(query.Get("IncludeItemTypes") == "Movie,Series")
				probe.recursiveSeen.Store(query.Get("Recursive") == "true")
				if strict && (!probe.searchTermSeen.Load() || !probe.includeTypesSeen.Load() || !probe.recursiveSeen.Load()) {
					w.WriteHeader(http.StatusBadRequest)
					return
				}
				_ = json.NewEncoder(w).Encode(map[string]any{"Items": []map[string]any{{"Id": label + "-movie", "Name": label + " Movie", "Type": "Movie"}}})
			case "/Users/" + userID + "/Items/Resume":
				_ = json.NewEncoder(w).Encode(map[string]any{"Items": []map[string]any{{"Id": label + "-resume", "Name": label + " Resume", "Type": "Movie"}}})
			default:
				http.NotFound(w, r)
			}
		}))
		t.Cleanup(probe.server.Close)
		return probe
	}

	normal := newUpstream("user-a", "Normal", false)
	strict := newUpstream("user-b", "Strict", true)
	withTempAppConfig(t, dualUpstreamConfig(normal.server.URL, strict.server.URL), func(app *App, handler http.Handler) {
		token := loginToken(t, handler, "secret")
		userPath := "/Users/" + app.Auth.ProxyUserID()
		for _, tc := range []struct {
			name, path, normalName, strictName string
		}{
			{"Views", userPath + "/Views", "Normal Library (A)", "Strict Library (B)"},
			{"ItemsSearch", userPath + "/Items?SearchTerm=movie&IncludeItemTypes=Movie,Series&Recursive=true", "Normal Movie", "Strict Movie"},
			{"Resume", userPath + "/Items/Resume?Limit=50", "Normal Resume", "Strict Resume"},
		} {
			t.Run(tc.name, func(t *testing.T) {
				normal.queryTokenSeen.Store(false)
				strict.queryTokenSeen.Store(false)
				strict.searchTermSeen.Store(false)
				strict.includeTypesSeen.Store(false)
				strict.recursiveSeen.Store(false)
				separator := "?"
				if strings.Contains(tc.path, "?") {
					separator = "&"
				}
				rr := doJSONRequest(t, handler, http.MethodGet, tc.path+separator+"X-Emby-Token="+url.QueryEscape(token), nil, token)
				if rr.Code != http.StatusOK {
					t.Fatalf("%s status = %d, want 200", tc.name, rr.Code)
				}
				names := itemNamesFromPayload(t, rr.Body.Bytes())
				if !hasName(names, tc.normalName) || !hasName(names, tc.strictName) {
					t.Errorf("%s merged names = %v, want both upstreams", tc.name, names)
				}
				if normal.queryTokenSeen.Load() || strict.queryTokenSeen.Load() {
					t.Errorf("%s leaked local X-Emby-Token query: normal=%v strict=%v", tc.name, normal.queryTokenSeen.Load(), strict.queryTokenSeen.Load())
				}
				if tc.name == "ItemsSearch" && (!strict.searchTermSeen.Load() || !strict.includeTypesSeen.Load() || !strict.recursiveSeen.Load()) {
					t.Errorf("strict upstream search query changed: SearchTerm=%v IncludeItemTypes=%v Recursive=%v", strict.searchTermSeen.Load(), strict.includeTypesSeen.Load(), strict.recursiveSeen.Load())
				}
			})
		}
	})
}
