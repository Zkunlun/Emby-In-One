package backend

import (
	"context"
	"encoding/json"
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
		rr := doJSONRequest(t, h, http.MethodPost, "/Sessions/Capabilities/Full?Id=mock-client-session", map[string]any{"PlayableMediaTypes": []string{"Video"}}, token)
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

// D05 is conditional: RC2 already stopped treating a requested Fields token
// as proof the upstream returned all media sources. The official enum policy
// will be frozen in 8F per endpoint after version-specific verification.
func TestPhase8BContractD05RequestedMediaSourcesDoesNotProveCompleteness(t *testing.T) {
	q := url.Values{"Fields": {"ProviderIds"}}
	complete := requestMergeFields(q)
	if complete {
		t.Fatal("D05 RC2 regression: requested Fields cannot establish complete MediaSources")
	}
	fields := strings.Split(q.Get("Fields"), ",")
	if !phase8BHasKey(fields, "MediaSources") {
		t.Fatalf("D05 test fixture did not exercise RC2 MediaSources request; Fields=%v", fields)
	}
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
	seen := [2]string{}
	mk := func(index int) (*phase8BRecorder, string) {
		srv, cap := phase8BUpstream(t, http.StatusNoContent, func(w http.ResponseWriter, r *http.Request) {
			if r.URL.Path == "/Users/AuthenticateByName" {
				_ = json.NewEncoder(w).Encode(map[string]any{"AccessToken": "mock-token", "User": map[string]any{"Id": "up-user"}})
				return
			}
			if r.URL.Path == "/Sessions/Capabilities" {
				seen[index] = r.URL.Query().Get("Id") // local fixture only; never logged
			}
			w.WriteHeader(http.StatusNoContent)
		})
		return cap, srv.URL
	}
	aRec, urlA := mk(0)
	bRec, urlB := mk(1)
	withTempAppConfig(t, dualUpstreamConfig(urlA, urlB), func(_ *App, h http.Handler) {
		token := loginToken(t, h, "secret")
		rr := doJSONRequest(t, h, http.MethodPost, "/Sessions/Capabilities?Id=frontend-session-unknown", nil, token)
		if rr.Code != http.StatusNoContent {
			t.Fatalf("D07 capabilities status=%d fixture failure", rr.Code)
		}
		if len(aRec.snapshot()) < 2 || len(bRec.snapshot()) < 2 {
			t.Fatalf("D07 expected login and capabilities per mock upstream")
		}
		if seen[0] == "frontend-session-unknown" && seen[1] == "frontend-session-unknown" {
			t.Errorf("D07 same unresolvable client Session Id broadcast to distinct upstreams")
		}
	})
}
func TestPhase8BContractD08BasicCapabilitiesUsesQueryNotJSONBody(t *testing.T) {
	base, rec := phase8BContractFixture(t)
	withTempAppConfig(t, singleUpstreamConfig(base), func(_ *App, h http.Handler) {
		token := loginToken(t, h, "secret")
		rr := doJSONRequest(t, h, http.MethodPost, "/Sessions/Capabilities?Id=frontend-session&SupportsSync=true",
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
	// Whether custom fields must have stable defaults remains an 8G design decision.
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
	if path != "/Videos/probe" {
		t.Errorf("D12 expected legacy split-route probe path, got different route category")
	}
	if ua != "Emby-In-One-Liveness/1.0" {
		t.Errorf("D12 probe identity semantics changed; reassess 8H")
	}
	if token != "" {
		t.Fatal("D12 anonymous probe leaked authentication token")
	}
	// GREEN locks historical route-reachability behavior; it is NOT proof a
	// valid API route was used or any authenticated media is playable.
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
