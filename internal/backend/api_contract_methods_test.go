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
