package backend

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
)

// phase8BContractFixture uses only httptest loopback services. The recorder
// never retains token, UserId, UA, path identity or JSON body values.
func phase8BContractFixture(t *testing.T) (string, *phase8BRecorder) {
	t.Helper()
	srv, rec := phase8BUpstream(t, http.StatusOK, func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/Users/AuthenticateByName":
			_ = json.NewEncoder(w).Encode(map[string]any{"AccessToken": "mock-token", "User": map[string]any{"Id": "mock-upstream-user"}})
		case "/Items/episode-1/PlaybackInfo":
			_ = json.NewEncoder(w).Encode(map[string]any{"MediaSources": []any{map[string]any{"Id": "source-1", "Container": "mkv"}}})
		case "/Videos/episode-1/stream.mkv":
			w.Header().Set("Content-Type", "video/x-matroska")
			w.WriteHeader(http.StatusOK)
			if r.Method != http.MethodHead {
				_, _ = w.Write([]byte("mock-video"))
			}
		default:
			w.WriteHeader(http.StatusNoContent)
		}
	})
	return srv.URL, rec
}
func phase8BRequireRecorded(t *testing.T, rec *phase8BRecorder, path string) phase8BWireShape {
	t.Helper()
	for _, r := range rec.snapshot() {
		if r.Path == path {
			return r
		}
	}
	t.Fatalf("upstream never received expected route template %s (fixture/routing error)", path)
	return phase8BWireShape{}
}
func phase8BHasKey(keys []string, key string) bool {
	for _, v := range keys {
		if v == key {
			return true
		}
	}
	return false
}
func TestPhase8BContractD01HeadPreservesUpstreamMethod(t *testing.T) {
	base, rec := phase8BContractFixture(t)
	withTempAppConfig(t, singleUpstreamConfig(base), func(a *App, h http.Handler) {
		token := loginToken(t, h, "secret")
		id := a.IDStore.GetOrCreateVirtualID("episode-1", a.Upstream.Clients()[0].ID)
		rr := doJSONRequest(t, h, http.MethodHead, "/Videos/"+id+"/stream.mkv", nil, token)
		if rr.Code != http.StatusOK {
			t.Fatalf("HEAD route status=%d, fixture failure", rr.Code)
		}
		got := phase8BRequireRecorded(t, rec, "/Videos/{segment}/{segment}")
		if got.Method != http.MethodHead {
			t.Errorf("D01 upstream Method=%s, want HEAD (client request HEAD)", got.Method)
		}
	})
}
func TestPhase8BContractD02GetPlaybackInfoHasNoBody(t *testing.T) {
	base, rec := phase8BContractFixture(t)
	withTempAppConfig(t, singleUpstreamConfig(base), func(a *App, h http.Handler) {
		token := loginToken(t, h, "secret")
		id := a.IDStore.GetOrCreateVirtualID("episode-1", a.Upstream.Clients()[0].ID)
		rr := doJSONRequest(t, h, http.MethodGet, "/Items/"+id+"/PlaybackInfo", nil, token)
		if rr.Code != http.StatusOK {
			t.Fatalf("GET playback status=%d, fixture failure", rr.Code)
		}
		got := phase8BRequireRecorded(t, rec, "/Items/{segment}/PlaybackInfo")
		if got.Method != http.MethodGet {
			t.Fatalf("GET playback sent %s, fixture failure", got.Method)
		}
		if len(got.BodyFields) > 0 {
			t.Errorf("D02 GET PlaybackInfo sent Body field types=%v; official GET must omit JSON Body", got.BodyFields)
		}
	})
}
func TestPhase8BContractD03FullCapabilitiesRetainsRequiredId(t *testing.T) {
	base, rec := phase8BContractFixture(t)
	withTempAppConfig(t, singleUpstreamConfig(base), func(a *App, h http.Handler) {
		token := loginToken(t, h, "secret")
		rr := doJSONRequest(t, h, http.MethodPost, "/Sessions/Capabilities/Full?Id="+url.QueryEscape(a.IDStore.GetOrCreateVirtualID("mock-client-session", a.Upstream.Clients()[0].ID)), map[string]any{"PlayableMediaTypes": []string{"Video"}}, token)
		if rr.Code != http.StatusNoContent {
			t.Fatalf("Capabilities/Full status=%d, fixture failure", rr.Code)
		}
		got := phase8BRequireRecorded(t, rec, "/Sessions/Capabilities/Full")
		if got.Method != http.MethodPost {
			t.Fatalf("Full method=%s", got.Method)
		}
		if !phase8BHasKey(got.QueryKeys, "Id") {
			t.Errorf("D03 Full Capabilities lost required Query Id")
		}
		if got.BodyFields["PlayableMediaTypes"] != "array" {
			t.Errorf("D03 Full Capabilities lost allowed JSON body: %v", got.BodyFields)
		}
	})
}
func TestPhase8BContractD04PostPlaybackInfoNoRedundantQueryUserId(t *testing.T) {
	base, rec := phase8BContractFixture(t)
	withTempAppConfig(t, singleUpstreamConfig(base), func(a *App, h http.Handler) {
		token := loginToken(t, h, "secret")
		id := a.IDStore.GetOrCreateVirtualID("episode-1", a.Upstream.Clients()[0].ID)
		rr := doJSONRequest(t, h, http.MethodPost, "/Items/"+id+"/PlaybackInfo", map[string]any{"UserId": a.Auth.ProxyUserID()}, token)
		if rr.Code != http.StatusOK {
			t.Fatalf("POST playback status=%d, fixture failure", rr.Code)
		}
		got := phase8BRequireRecorded(t, rec, "/Items/{segment}/PlaybackInfo")
		if got.BodyFields["UserId"] != "string" {
			t.Fatalf("POST playback lost required Body UserId: %v", got.BodyFields)
		}
		if phase8BHasKey(got.QueryKeys, "UserId") || phase8BHasKey(got.QueryKeys, "MediaSourceId") {
			t.Errorf("D04 POST PlaybackInfo injected body-only Query fields: %v", got.QueryKeys)
		}
	})
}

// D05: MediaSources is a BaseItemDto response field but is not an allowed
// Fields selector in Emby's ItemsService list API.
func TestPhase8BContractD05RequestedMediaSourcesDoesNotProveCompleteness(t *testing.T) {
	for _, tc := range []struct{ fields, want string }{
		{"ProviderIds", "ProviderIds"},
		{"Overview,MediaSources,MediaStreams", "Overview,MediaStreams,ProviderIds"},
		{"mediasources,ProviderIds,MEDIASOURCES", "ProviderIds"},
		{"", "ProviderIds"},
	} {
		t.Run(tc.fields, func(t *testing.T) {
			q := url.Values{"Fields": {tc.fields}}
			if requestMergeFields(q) {
				t.Fatal("D05 Fields cannot prove source completeness")
			}
			if got := q.Get("Fields"); got != tc.want {
				t.Errorf("D05 Fields=%q, want %q", got, tc.want)
			}
		})
	}
	q := url.Values{"Fields": {"Overview,MediaSources"}, "fields": {"MediaStreams,ProviderIds"}}
	if requestMergeFields(q) {
		t.Fatal("D05 case-alias selector must not establish completeness")
	}
	if q.Get("Fields") != "Overview,MediaStreams,ProviderIds" || q.Has("fields") {
		t.Errorf("D05 conflicting Fields aliases not normalized deterministically")
	}
}

// Path-scoped Users Items retains upstream user and ParentId while dropping
// every case variation of a user ID Query argument.
func TestPhase8FUserItemsScopedParentDoesNotSendUserQueryAlias(t *testing.T) {
	var seenPath string
	var seenQuery url.Values
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/Users/AuthenticateByName" {
			_ = json.NewEncoder(w).Encode(map[string]any{"AccessToken": "mock-token", "User": map[string]any{"Id": "up-user"}})
			return
		}
		if r.URL.Path == "/Users/up-user/Items" {
			seenPath, seenQuery = r.URL.Path, r.URL.Query()
			_ = json.NewEncoder(w).Encode(map[string]any{"Items": []any{}, "TotalRecordCount": 0})
			return
		}
		http.NotFound(w, r)
	}))
	t.Cleanup(upstream.Close)
	withTempAppConfig(t, singleUpstreamConfig(upstream.URL), func(a *App, h http.Handler) {
		token := loginToken(t, h, "secret")
		user := a.Auth.ProxyUserID()
		parent := a.IDStore.GetOrCreateVirtualID("up-library", a.Upstream.Clients()[0].ID)
		path := "/Users/" + user + "/Items?ParentId=" + url.QueryEscape(parent) + "&UserId=" + url.QueryEscape(user) + "&userid=client-spoof&Limit=1"
		rr := doJSONRequest(t, h, http.MethodGet, path, nil, token)
		if rr.Code != http.StatusOK {
			t.Fatalf("D06 scoped fixture status=%d", rr.Code)
		}
		if seenPath != "/Users/up-user/Items" || seenQuery.Get("ParentId") != "up-library" {
			t.Errorf("D06 upstream user or parent scope changed")
		}
		for key := range seenQuery {
			if strings.EqualFold(key, "UserId") {
				t.Errorf("D06 Users Items forwarded redundant Query %s", key)
			}
		}
	})
}

func TestPhase8BContractD06UserItemsDoesNotRepeatUserIdInQuery(t *testing.T) {
	srv, rec := phase8BUpstream(t, http.StatusOK, func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/Users/AuthenticateByName" {
			_ = json.NewEncoder(w).Encode(map[string]any{"AccessToken": "mock-token", "User": map[string]any{"Id": "up-user"}})
			return
		}
		if r.URL.Path == "/Users/up-user/Items" || r.URL.Path == "/Items" {
			_ = json.NewEncoder(w).Encode(map[string]any{"Items": []any{}, "TotalRecordCount": 0})
			return
		}
		w.WriteHeader(http.StatusNoContent)
	})
	withTempAppConfig(t, singleUpstreamConfig(srv.URL), func(a *App, h http.Handler) {
		token := loginToken(t, h, "secret")
		reqPath := "/Users/" + a.Auth.ProxyUserID() + "/Items?Limit=1"
		rr := doJSONRequest(t, h, http.MethodGet, reqPath, nil, token)
		if rr.Code != http.StatusOK {
			t.Fatalf("D06 GET Users Items status=%d fixture/routing failure", rr.Code)
		}
		got := phase8BRequireRecorded(t, rec, "/Users/{segment}/Items")
		if phase8BHasKey(got.QueryKeys, "UserId") {
			t.Errorf("D06 UserId already exists in path, redundant upstream Query UserId: %v", got.QueryKeys)
		}
	})
}

func TestPhase8BContractD07CapabilitiesNeverBroadcastSameUnknownSessionId(t *testing.T) {
	seen := [2][]string{}
	mk := func(index int) string {
		srv, _ := phase8BUpstream(t, http.StatusNoContent, func(w http.ResponseWriter, r *http.Request) {
			if r.URL.Path == "/Users/AuthenticateByName" {
				_ = json.NewEncoder(w).Encode(map[string]any{"AccessToken": "mock-token", "User": map[string]any{"Id": "up-user"}})
				return
			}
			if r.URL.Path == "/Sessions/Capabilities" || r.URL.Path == "/Sessions/Capabilities/Full" {
				seen[index] = append(seen[index], r.URL.Query().Get("Id"))
			}
			w.WriteHeader(http.StatusNoContent)
		})
		return srv.URL
	}
	urlA, urlB := mk(0), mk(1)
	withTempAppConfig(t, dualUpstreamConfig(urlA, urlB), func(a *App, h http.Handler) {
		token := loginToken(t, h, "secret")
		sessionA := a.IDStore.GetOrCreateVirtualID("source-a-session", a.Upstream.Clients()[0].ID)
		sessionB := a.IDStore.GetOrCreateVirtualID("source-b-session", a.Upstream.Clients()[1].ID)
		for _, input := range []struct {
			target           string
			expectA, expectB int
		}{
			{"/Sessions/Capabilities?Id=totally-unknown-session", 0, 0},
			{"/Sessions/Capabilities?Id=&id=" + url.QueryEscape(sessionA), 0, 0},
			{"/Sessions/Capabilities?Id=" + url.QueryEscape(sessionA) + "&id=foreign-session", 0, 0},
			{"/Sessions/Capabilities?Id=" + url.QueryEscape(sessionA), 1, 0},
			{"/Sessions/Capabilities/Full?Id=" + url.QueryEscape(sessionB), 1, 1},
		} {
			rr := doJSONRequest(t, h, http.MethodPost, input.target, map[string]any{"SupportsMediaControl": true}, token)
			if rr.Code != http.StatusNoContent {
				t.Fatalf("D07 status=%d fixture failure", rr.Code)
			}
			if len(seen[0]) != input.expectA || len(seen[1]) != input.expectB {
				t.Errorf("D07 capability routing counts A=%d B=%d; wanted %d/%d", len(seen[0]), len(seen[1]), input.expectA, input.expectB)
			}
		}
		if len(seen[0]) > 0 && seen[0][0] != "source-a-session" {
			t.Errorf("D07 source A received wrong session Id")
		}
		if len(seen[1]) > 0 && seen[1][0] != "source-b-session" {
			t.Errorf("D07 source B received wrong session Id")
		}
	})
}
func TestPhase8BContractD08BasicCapabilitiesUsesQueryNotJSONBody(t *testing.T) {
	base, rec := phase8BContractFixture(t)
	withTempAppConfig(t, singleUpstreamConfig(base), func(a *App, h http.Handler) {
		token := loginToken(t, h, "secret")
		session := a.IDStore.GetOrCreateVirtualID("frontend-session", a.Upstream.Clients()[0].ID)
		rr := doJSONRequest(t, h, http.MethodPost, "/Sessions/Capabilities?Id="+url.QueryEscape(session)+"&SupportsSync=true",
			map[string]any{"SupportsMediaControl": true}, token)
		if rr.Code != http.StatusNoContent {
			t.Fatalf("D08 capabilities status=%d fixture failure", rr.Code)
		}
		got := phase8BRequireRecorded(t, rec, "/Sessions/Capabilities")
		if len(got.BodyFields) > 0 {
			t.Errorf("D08 basic capabilities transmitted JSON body fields=%v; official basic contract uses Query", got.BodyFields)
		}
		if !phase8BHasKey(got.QueryKeys, "Id") {
			t.Fatalf("D08 upstream lost basic capabilities Id; fixture not suitable")
		}
		if !phase8BHasKey(got.QueryKeys, "SupportsMediaControl") {
			t.Errorf("D08 basic body legacy capability flag not normalized to Query")
		}
	})
}

func TestPhase8BContractD09SessionEventTranslatesSessionId(t *testing.T) {
	upstreamSessionID := ""
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/Users/AuthenticateByName" {
			_ = json.NewEncoder(w).Encode(map[string]any{"AccessToken": "mock-token", "User": map[string]any{"Id": "up-user"}})
			return
		}
		if r.URL.Path == "/Sessions/Playing" {
			var body map[string]any
			if err := json.NewDecoder(r.Body).Decode(&body); err == nil {
				upstreamSessionID, _ = body["SessionId"].(string)
			}
			w.WriteHeader(http.StatusNoContent)
			return
		}
		http.NotFound(w, r)
	}))
	t.Cleanup(srv.Close)
	withTempAppConfig(t, singleUpstreamConfig(srv.URL), func(a *App, h http.Handler) {
		token := loginToken(t, h, "secret")
		client := a.Upstream.Clients()[0]
		item := a.IDStore.GetOrCreateVirtualID("up-item", client.ID)
		media := a.IDStore.GetOrCreateVirtualID("up-media", client.ID)
		play := a.IDStore.GetOrCreateVirtualID("up-play", client.ID)
		session := a.IDStore.GetOrCreateVirtualID("up-session", client.ID)
		rr := doJSONRequest(t, h, http.MethodPost, "/Sessions/Playing", map[string]any{
			"ItemId": item, "MediaSourceId": media, "PlaySessionId": play, "SessionId": session, "PositionTicks": 1,
		}, token)
		if rr.Code != http.StatusNoContent {
			t.Fatalf("D09 fixture playback event HTTP %d (cannot assess mapping)", rr.Code)
		}
		if upstreamSessionID != "up-session" {
			t.Errorf("D09 upstream SessionId was not translated from virtual value to original value (forwarded original=%t)", upstreamSessionID == "up-session")
		}
	})
}
func TestPhase8BContractD10PositionTicksJSONInt64Preserved(t *testing.T) {
	req := httptest.NewRequest(http.MethodPost, "/Sessions/Playing/Progress",
		strings.NewReader(`{"PositionTicks":9007199254740993,"Nested":{"RunTimeTicks":9007199254740993}}`))
	value, err := decodeOptionalJSON(req)
	if err != nil {
		t.Fatal(err)
	}
	wire, err := json.Marshal(value)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(wire), `"PositionTicks":9007199254740993`) ||
		!strings.Contains(string(wire), `"RunTimeTicks":9007199254740993`) {
		t.Errorf("D10 64-bit JSON integer rounded in decode/encode path; want exact >2^53")
	}
}
func TestPhase8BContractD11CustomMissingIdentityCannotLeakRealClient(t *testing.T) {
	cli := newUpstreamClient(Config{}, UpstreamConfig{SpoofClient: "custom", CustomClient: "mock-custom"}, 0, nil)
	identity, ok := cli.configuredOutboundIdentity()
	if !ok {
		t.Fatal("D11 fixture custom identity unavailable")
	}
	original := http.Header{}
	original.Set("User-Agent", "REAL-PRIVATE-UA")
	original.Set("X-Emby-Device-Id", "REAL-PRIVATE-DEVICE")
	original.Set("X-Emby-Client-Version", "REAL-PRIVATE-VERSION")
	headers := identity.applyHeaders(original)
	for _, name := range []string{"User-Agent", "X-Emby-Device-Id", "X-Emby-Client-Version"} {
		if strings.Contains(headers.Get(name), "REAL-PRIVATE") {
			t.Errorf("D11 custom missing field leaked original %s", name)
		}
	}
	for _, name := range []string{"User-Agent", "X-Emby-Client", "X-Emby-Client-Version", "X-Emby-Device-Name", "X-Emby-Device-Id"} {
		if headers.Get(name) == "" {
			t.Errorf("D11 expected a non-empty identity field: %s", name)
		}
	}

}
func TestPhase8BContractD12ProbeIsAnonymousRouteReachabilityOnly(t *testing.T) {
	var path, ua, token string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		path = r.URL.Path
		ua = r.Header.Get("User-Agent")
		token = r.Header.Get("X-Emby-Token")
		w.WriteHeader(http.StatusNotFound)
	}))
	t.Cleanup(server.Close)
	reachable := probeOneStreamBase(context.Background(), server.Client(), server.URL+streamLivenessProbePath)
	if !reachable {
		t.Error("D12 legacy 404 route proves path is reachable, not playable")
	}
	if path != "/Videos/00000000000000000000000000000000/stream" {
		t.Errorf("D12 probe must use documented Emby stream route shape, got %q", path)
	}
	if ua != "Emby-In-One-Liveness/1.0" {
		t.Errorf("D12 probe identity semantics changed; reassess 8H")
	}
	if token != "" {
		t.Fatal("D12 anonymous probe leaked authentication token")
	}
	// GREEN only establishes anonymous HTTP reachability at the official route
	// shape; the invalid item ID cannot prove authenticated media playback.
}

func TestPhase8CHeadAudioVideoStatusesRangesAndHLS(t *testing.T) {
	cases := []struct {
		name, method, path, contentType, requestRange, responseBody string
		status                                                      int
	}{
		{"video-head-200", http.MethodHead, "/Videos/item-original/stream.mkv", "video/x-matroska", "", "", http.StatusOK},
		{"audio-head-206", http.MethodHead, "/Audio/item-original/stream.mp3", "audio/mpeg", "bytes=0-6", "", http.StatusPartialContent},
		{"video-head-404", http.MethodHead, "/Videos/item-original/not-found.mkv", "video/x-matroska", "", "", http.StatusNotFound},
		{"video-hls-head", http.MethodHead, "/Videos/item-original/master.m3u8", "application/vnd.apple.mpegurl", "", "", http.StatusOK},
		{"audio-get-200", http.MethodGet, "/Audio/item-original/stream.mp3", "audio/mpeg", "", "audio-body", http.StatusOK},
		{"video-get-206", http.MethodGet, "/Videos/item-original/stream.mkv", "video/x-matroska", "bytes=0-6", "partial", http.StatusPartialContent},
		{"video-hls-get", http.MethodGet, "/Videos/item-original/master.m3u8", "application/vnd.apple.mpegurl", "", "#EXTM3U\n#EXTINF:5,\nseg.ts\n", http.StatusOK},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var seenMethod, seenRange string
			upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.Method == http.MethodPost && r.URL.Path == "/Users/AuthenticateByName" {
					_ = json.NewEncoder(w).Encode(map[string]any{"AccessToken": "mock-token", "User": map[string]any{"Id": "up-user"}})
					return
				}
				if r.URL.Path != tc.path {
					http.NotFound(w, r)
					return
				}
				seenMethod = r.Method
				seenRange = r.Header.Get("Range")
				w.Header().Set("Content-Type", tc.contentType)
				w.Header().Set("Accept-Ranges", "bytes")
				w.Header().Set("ETag", `"mock-etag"`)
				if tc.method == http.MethodHead {
					w.Header().Set("Content-Length", "12345")
				}
				if tc.status == http.StatusPartialContent {
					w.Header().Set("Content-Range", "bytes 0-6/100")
				}
				w.WriteHeader(tc.status)
				if r.Method != http.MethodHead {
					_, _ = w.Write([]byte(tc.responseBody))
				}
			}))
			t.Cleanup(upstream.Close)
			withTempAppConfig(t, singleUpstreamConfig(upstream.URL), func(app *App, h http.Handler) {
				token := loginToken(t, h, "secret")
				id := app.IDStore.GetOrCreateVirtualID("item-original", app.Upstream.Clients()[0].ID)
				path := strings.Replace(tc.path, "/item-original/", "/"+id+"/", 1)
				req := httptest.NewRequest(tc.method, path, nil)
				req.Header.Set("X-Emby-Token", token)
				if tc.requestRange != "" {
					req.Header.Set("Range", tc.requestRange)
				}
				rr := httptest.NewRecorder()
				h.ServeHTTP(rr, req)
				if seenMethod != tc.method {
					t.Errorf("upstream method=%q want=%q (must preserve client method)", seenMethod, tc.method)
				}
				if seenRange != tc.requestRange {
					t.Errorf("upstream Range=%q want=%q", seenRange, tc.requestRange)
				}
				if rr.Code != tc.status {
					t.Errorf("downstream status=%d want=%d", rr.Code, tc.status)
				}
				if tc.method == http.MethodHead {
					if rr.Body.Len() != 0 {
						t.Errorf("HEAD forwarded %d body bytes", rr.Body.Len())
					}
					for _, name := range []string{"Content-Type", "Content-Length", "Accept-Ranges", "ETag"} {
						if rr.Header().Get(name) == "" {
							t.Errorf("HEAD dropped response header %s", name)
						}
					}
					if tc.status == http.StatusPartialContent && rr.Header().Get("Content-Range") != "bytes 0-6/100" {
						t.Errorf("HEAD 206 lost Content-Range")
					}
				} else if strings.HasSuffix(tc.path, ".m3u8") {
					if !strings.Contains(rr.Body.String(), "#EXTM3U") {
						t.Errorf("GET HLS manifest missing, body=%q", rr.Body.String())
					}
				} else if rr.Body.String() != tc.responseBody {
					t.Errorf("GET body=%q want=%q", rr.Body.String(), tc.responseBody)
				}
			})
		})
	}
}

func TestPhase8DPlaybackInfoMethodSpecificUserAndSelectedSource(t *testing.T) {
	type requestShape struct {
		method string
		query  url.Values
		body   map[string]any
	}
	var captured []requestShape
	full := task6HTTPMovie("mock-movie", task6HTTPSource("source-a", 1000), task6HTTPSource("source-b", 1000))
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch {
		case r.Method == http.MethodPost && r.URL.Path == "/Users/AuthenticateByName":
			_ = json.NewEncoder(w).Encode(map[string]any{"AccessToken": "mock-token", "User": map[string]any{"Id": "actual-upstream-user"}})
		case r.URL.Path == "/Items/mock-movie/PlaybackInfo":
			var payload map[string]any
			_ = json.NewDecoder(r.Body).Decode(&payload)
			captured = append(captured, requestShape{method: r.Method, query: r.URL.Query(), body: payload})
			source := ""
			if payload != nil {
				source, _ = payload["MediaSourceId"].(string)
			}
			sources := []any{}
			for _, item := range asItems(full["MediaSources"]) {
				if source == "" || source == item["Id"] {
					sources = append(sources, item)
				}
			}
			_ = json.NewEncoder(w).Encode(map[string]any{"MediaSources": sources, "PlaySessionId": "mock-session"})
		case r.URL.Path == "/Items/mock-movie" || r.URL.Path == "/Users/actual-upstream-user/Items/mock-movie":
			_ = json.NewEncoder(w).Encode(full)
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(upstream.Close)
	withTempAppConfig(t, phase2SNTPFixtureConfig(upstream.URL), func(app *App, h http.Handler) {
		token := loginToken(t, h, "secret")
		client := app.Upstream.Clients()[0]
		virtualItem, err := app.IDStore.RegisterMergeItem(newMergeCandidate(client.ID, deepCloneMap(full), nil, true), "")
		if err != nil {
			t.Fatal(err)
		}
		virtualSource, err := app.IDStore.GetMergeMediaSourceID(client.ID, "mock-movie", "source-a")
		if err != nil {
			t.Fatal(err)
		}
		virtualUser := app.Auth.ProxyUserID()
		getRR := doJSONRequest(t, h, http.MethodGet, "/Items/"+virtualItem+"/PlaybackInfo?UserId="+url.QueryEscape(virtualUser), nil, token)
		if getRR.Code != http.StatusOK {
			t.Fatalf("GET fixture playback status=%d", getRR.Code)
		}
		postRR := doJSONRequest(t, h, http.MethodPost, "/Items/"+virtualItem+"/PlaybackInfo?UserId="+url.QueryEscape(virtualUser)+"&userid="+url.QueryEscape(virtualUser)+"&MediaSourceId="+url.QueryEscape(virtualSource),
			map[string]any{"UserId": virtualUser, "MediaSourceId": virtualSource, "EnableDirectPlay": true}, token)
		if postRR.Code != http.StatusOK {
			t.Fatalf("POST fixture playback status=%d", postRR.Code)
		}
		if len(captured) != 2 {
			t.Fatalf("PlaybackInfo upstream calls=%d, want 2", len(captured))
		}
		get, post := captured[0], captured[1]
		if get.method != http.MethodGet || len(get.body) != 0 {
			t.Errorf("GET sent unexpected JSON body fields: %d", len(get.body))
		}
		if get.query.Get("UserId") != "actual-upstream-user" {
			t.Errorf("GET lost required upstream UserId Query")
		}
		if post.method != http.MethodPost {
			t.Fatalf("expected POST upstream, got %s", post.method)
		}
		for key := range post.query {
			if strings.EqualFold(key, "UserId") || strings.EqualFold(key, "MediaSourceId") {
				t.Errorf("POST sends Body-only query key %q", key)
			}
		}
		if post.body["UserId"] != "actual-upstream-user" || post.body["MediaSourceId"] != "source-a" || post.body["EnableDirectPlay"] != true {
			t.Errorf("POST body lost real upstream UserId, selected source or player flags")
		}
		var response map[string]any
		if err := json.Unmarshal(postRR.Body.Bytes(), &response); err != nil {
			t.Fatal(err)
		}
		selected := asItems(map[string]any{"Items": response["MediaSources"]})
		if len(selected) != 1 || selected[0]["Id"] != virtualSource {
			t.Errorf("selected playback did not retain single virtual MediaSourceId")
		}
	})
}

func TestPhase8ESessionIdProvenanceAcrossPlaybackEvents(t *testing.T) {
	type observed struct{ route, session string }
	sentA := make(chan observed, 32)
	sentB := make(chan observed, 32)
	newServer := func(index int) *httptest.Server {
		return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if r.URL.Path == "/Users/AuthenticateByName" {
				_ = json.NewEncoder(w).Encode(map[string]any{"AccessToken": "fixture-token", "User": map[string]any{"Id": "fixture-user"}})
				return
			}
			if strings.HasPrefix(r.URL.Path, "/Sessions/Playing") {
				var payload map[string]any
				if err := json.NewDecoder(r.Body).Decode(&payload); err != nil {
					t.Errorf("bad event body: %v", err)
				}
				value, _ := payload["SessionId"].(string)
				item := observed{route: r.URL.Path, session: value}
				if index == 0 {
					sentA <- item
				} else {
					sentB <- item
				}
				w.WriteHeader(http.StatusNoContent)
				return
			}
			http.NotFound(w, r)
		}))
	}
	aServer, bServer := newServer(0), newServer(1)
	t.Cleanup(aServer.Close)
	t.Cleanup(bServer.Close)
	withTempAppConfig(t, dualUpstreamConfig(aServer.URL, bServer.URL), func(a *App, h http.Handler) {
		token := loginToken(t, h, "secret")
		first, second := a.Upstream.Clients()[0], a.Upstream.Clients()[1]
		item := a.IDStore.GetOrCreateVirtualID("item-a", first.ID)
		media := a.IDStore.GetOrCreateVirtualID("media-a", first.ID)
		play := a.IDStore.GetOrCreateVirtualID("play-a", first.ID)
		local := a.IDStore.GetOrCreateVirtualID("source-a-session", first.ID)
		foreign := a.IDStore.GetOrCreateVirtualID("source-b-session", second.ID)
		for _, route := range []string{"/Sessions/Playing", "/Sessions/Playing/Progress", "/Sessions/Playing/Stopped"} {
			for _, tc := range []struct{ name, session, want string }{
				{"mapped", local, "source-a-session"},
				{"foreign", foreign, ""},
				{"unknown", "unmapped-session-id", ""},
			} {
				t.Run(strings.TrimPrefix(route, "/Sessions/")+"/"+tc.name, func(t *testing.T) {
					rr := doJSONRequest(t, h, http.MethodPost, route, map[string]any{
						"ItemId": item, "MediaSourceId": media, "PlaySessionId": play,
						"SessionId": tc.session, "PositionTicks": 20,
					}, token)
					if rr.Code != http.StatusNoContent {
						t.Fatalf("fixture Session event HTTP=%d", rr.Code)
					}
					select {
					case got := <-sentA:
						if got.route != route || got.session != tc.want {
							t.Errorf("D09 forwarded route/session mismatch: route=%s session-matches=%t; wanted route=%s known=%t",
								got.route, got.session == tc.want, route, tc.want != "")
						}
					default:
						t.Fatal("fixture did not observe authorized upstream event")
					}
					select {
					case <-sentB:
						t.Error("D09 event leaked to foreign upstream")
					default:
					}
				})
			}
		}
	})
}

func TestPhase8FGlobalItemsSearchFallbackKeepsCorrectUpstreamUserScope(t *testing.T) {
	var globalQuery, userQuery url.Values
	var userPath string
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/Users/AuthenticateByName":
			_ = json.NewEncoder(w).Encode(map[string]any{"AccessToken": "mock-token", "User": map[string]any{"Id": "up-user"}})
		case "/Items":
			globalQuery = cloneValues(r.URL.Query())
			_ = json.NewEncoder(w).Encode(map[string]any{"Items": []any{}, "TotalRecordCount": 0})
		case "/Users/up-user/Items":
			userPath, userQuery = r.URL.Path, cloneValues(r.URL.Query())
			_ = json.NewEncoder(w).Encode(map[string]any{"Items": []any{}, "TotalRecordCount": 0})
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(upstream.Close)
	withTempAppConfig(t, singleUpstreamConfig(upstream.URL), func(a *App, h http.Handler) {
		token := loginToken(t, h, "secret")
		virtualUser := a.Auth.ProxyUserID()
		path := "/Items?SearchTerm=search-probe&Limit=3&UserId=" + url.QueryEscape(virtualUser) + "&userid=spoof"
		rr := doJSONRequest(t, h, http.MethodGet, path, nil, token)
		if rr.Code != http.StatusOK {
			t.Fatalf("D06 global search fixture HTTP=%d", rr.Code)
		}
		if globalQuery.Get("UserId") != "up-user" {
			t.Error("D06 /Items global Query must retain correct upstream real UserId")
		}
		if userPath != "/Users/up-user/Items" || userQuery.Get("SearchTerm") != "search-probe" {
			t.Errorf("D06 search compatibility fallback lost route or search")
		}
		for key := range userQuery {
			if strings.EqualFold(key, "UserId") {
				t.Errorf("D06 /Users/{id}/Items sibling fallback has redundant UserId Query")
			}
		}
	})
}

func TestPhase8GExactNumbersThroughJSONBoundaries(t *testing.T) {
	const raw = `{"PositionTicks":9007199254740993,"Nested":{"RunTimeTicks":9223372036854775807},"PlaybackRate":1.2500000000000001,"NoValue":null}`
	wantTokens := []string{`"PositionTicks":9007199254740993`, `"RunTimeTicks":9223372036854775807`, `"PlaybackRate":1.2500000000000001`}
	check := func(name string, body any) {
		t.Helper()
		out, err := json.Marshal(body)
		if err != nil {
			t.Fatalf("%s marshal: %v", name, err)
		}
		for _, token := range wantTokens {
			if !strings.Contains(string(out), token) {
				t.Errorf("%s lost exact JSON number %s", name, token)
			}
		}
	}
	t.Run("session-request", func(t *testing.T) {
		req := httptest.NewRequest(http.MethodPost, "/Sessions/Playing/Progress", strings.NewReader(raw))
		body, err := decodeOptionalJSON(req)
		if err != nil {
			t.Fatal(err)
		}
		check("session-request", body)
		got, ok := numericInt64(body.(map[string]any)["PositionTicks"])
		if !ok || got != 9007199254740993 {
			t.Errorf("numericInt64 exact PositionTicks failed: %d %t", got, ok)
		}
	})
	t.Run("fallback-request", func(t *testing.T) {
		req := httptest.NewRequest(http.MethodPost, "/Plugins/PrivateOperation", strings.NewReader(raw))
		req.Header.Set("Content-Type", "application/json")
		body, err := decodeFallbackBody(req)
		if err != nil {
			t.Fatal(err)
		}
		check("fallback-request", body)
	})
	t.Run("session-upstream-response", func(t *testing.T) {
		resp := &http.Response{StatusCode: http.StatusOK, Body: io.NopCloser(strings.NewReader(raw))}
		status, body, err := readUpstreamJSONOrNoContent(resp)
		if err != nil || status != http.StatusOK {
			t.Fatalf("session upstream status=%d err=%v", status, err)
		}
		check("session-upstream-response", body)
	})
	t.Run("items-upstream-response", func(t *testing.T) {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Content-Type", "application/json")
			_, _ = io.WriteString(w, raw)
		}))
		t.Cleanup(srv.Close)
		c := newUpstreamClient(Config{}, UpstreamConfig{URL: srv.URL, SpoofClient: "none"}, 0, nil)
		c.setOnline("fixture-token", "fixture-user")
		body, err := c.RequestJSON(context.Background(), nil, nil, http.MethodGet, "/Items/probe", nil, nil)
		if err != nil {
			t.Fatal(err)
		}
		check("items-upstream-response", body)
	})
	t.Run("fraction-not-rounded-to-integer", func(t *testing.T) {
		if _, ok := numericInt64(json.Number("1.25")); ok {
			t.Error("D10 fractional number must not be accepted as integer ticks")
		}
	})
}

func TestPhase8GItemNumberReadsPreservedJSONNumber(t *testing.T) {
	for _, tc := range []struct {
		input json.Number
		want  float64
	}{
		{json.Number("42"), 42},
		{json.Number("1.25"), 1.25},
	} {
		if got := itemNumber(map[string]any{"SortField": tc.input}, "SortField"); got != tc.want {
			t.Errorf("UseNumber-backed sort value %s = %v, want %v", tc.input, got, tc.want)
		}
	}
}
