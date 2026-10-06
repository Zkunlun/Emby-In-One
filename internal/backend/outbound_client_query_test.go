package backend

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"reflect"
	"strings"
	"sync"
	"testing"
)

func queryIdentityConflicts() url.Values {
	return url.Values{
		"X-Emby-Client": {"", "REAL-CLIENT"}, "x-emby-client": {"SECOND-CLIENT"},
		"X-Emby-Client-Version": {"REAL-VERSION"}, "X-EMBY-CLIENT-VERSION": {"OTHER"},
		"X-Emby-Device-Name": {"REAL-DEVICE"}, "x-emby-device-name": {"OTHER"},
		"X-Emby-Device-Id": {"REAL-UA%2Fencoded", "REAL-ID"}, "x-emby-device-id": {"OTHER"},
		"DeviceId": {"", "REAL-ID"}, "deviceID": {"REAL-UA/encoded"},
		"Client": {"settings-client"}, "Version": {"package-version"}, "Device": {"business-device"},
		"UA": {"opaque-ua"}, "UserAgent": {"opaque-useragent"}, "User-Agent": {"opaque-user-agent"},
		"X-Emby-DeviceId": {"unknown-alias"}, "Id": {"target-id"},
		"MediaSourceId": {"media-source"}, "PlaySessionId": {"play-session"},
		"marker": {"a+b %2F 中文", "second"},
	}
}

func assertClientQuery(t *testing.T, got url.Values, want map[string]string) {
	t.Helper()
	for _, key := range []string{"X-Emby-Client", "X-Emby-Client-Version", "X-Emby-Device-Name", "X-Emby-Device-Id", "DeviceId"} {
		expected, checked := want[key]
		if !checked {
			continue
		}
		count := 0
		for spelling, values := range got {
			if !strings.EqualFold(spelling, key) {
				continue
			}
			if spelling != key || expected == "" || len(values) != 1 || values[0] != expected {
				t.Fatalf("query %s = %v, want canonical %s=%q", spelling, values, key, expected)
			}
			count++
		}
		expectedCount := 1
		if expected == "" {
			expectedCount = 0
		}
		if count != expectedCount {
			t.Fatalf("query %s occurrences=%d, want %d; %v", key, count, expectedCount, got)
		}
	}
}

func TestOutboundClientQueryMergedWire(t *testing.T) {
	records := make(chan outboundClientHeaderRecord, 1)
	stub := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		records <- recordClientHeaders(r)
		_, _ = io.WriteString(w, "{}")
	}))
	defer stub.Close()
	// A deployment prefix must not interfere with business endpoint matching.
	c := newUpstreamClient(Config{}, UpstreamConfig{URL: stub.URL + "/deploy/Users/prefix", SpoofClient: "infuse"}, 0, nil)
	c.setOnline("UPSTREAM-TOKEN", "UPSTREAM-USER")
	params := queryIdentityConflicts()
	params.Set("api_key", "LOCAL-TOKEN")
	params.Set("UserId", "LOCAL-USER")
	before := copyValues(params)
	body := rawRequestBody{data: []byte("opaque body: 9007199254740993"), contentType: "application/octet-stream"}
	headers := realClientConflictHeaders()
	headersBefore := headers.Clone()
	ctx := &RequestContext{Headers: headers.Clone(), LegacyProxyUserID: "LOCAL-USER"}
	resp, err := c.doRequest(context.Background(), ctx, http.MethodGet,
		"/Videos/item/stream.mp4?X-EMBY-CLIENT=PATH-CLIENT&deviceid=PATH-UA&marker=from-path",
		params, body, headers, true)
	if err != nil {
		t.Fatal(err)
	}
	_ = resp.Body.Close()
	got := <-records
	want := infuseClientHeaderWant()
	want["DeviceId"] = want["X-Emby-Device-Id"]
	assertClientQuery(t, got.query, want)
	assertClientHeaders(t, got.headers, want, "UPSTREAM-USER", "UPSTREAM-TOKEN")
	if got.path != "/deploy/Users/prefix/Videos/item/stream.mp4" ||
		got.query.Get("UserId") != "UPSTREAM-USER" || got.query.Get("api_key") != "UPSTREAM-TOKEN" ||
		got.body != string(body.data) {
		t.Fatalf("business/auth/body changed: %+v", got)
	}
	for key, values := range before {
		if strings.HasPrefix(strings.ToLower(key), "x-emby-") && key != "X-Emby-DeviceId" ||
			strings.EqualFold(key, "DeviceId") || key == "api_key" || key == "UserId" || key == "marker" {
			continue
		}
		if !reflect.DeepEqual(got.query[key], values) {
			t.Fatalf("business %s changed: %v", key, got.query[key])
		}
	}
	if !reflect.DeepEqual(got.query["marker"], []string{"from-path", "a+b %2F 中文", "second"}) {
		t.Fatalf("encoding/merge changed: %v", got.query["marker"])
	}
	if !reflect.DeepEqual(params, before) || !reflect.DeepEqual(headers, headersBefore) || !reflect.DeepEqual(ctx.Headers, headersBefore) {
		t.Fatal("source request mutated")
	}
}

func TestOutboundClientQueryEndpointBoundaries(t *testing.T) {
	records := make(chan outboundClientHeaderRecord, 1)
	stub := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		records <- recordClientHeaders(r)
		_, _ = io.WriteString(w, "{}")
	}))
	defer stub.Close()
	c := newUpstreamClient(Config{}, UpstreamConfig{URL: stub.URL, SpoofClient: "infuse"}, 0, nil)
	c.setOnline("UPSTREAM-TOKEN", "UPSTREAM-USER")
	type endpoint struct {
		name, method, path string
		stream, normalize  bool
	}
	cases := []endpoint{
		{"cancel-delete", "DELETE", "/Videos/ActiveEncodings", false, true},
		{"cancel-post", "POST", "/Videos/ActiveEncodings/Delete", false, true},
		{"cancel-wrong-method", "GET", "/Videos/ActiveEncodings", false, false},
		{"cancel-post-wrong-method", "GET", "/Videos/ActiveEncodings/Delete", true, false},
		{"sessions-filter", "GET", "/Sessions", false, false},
		{"playqueue-filter", "GET", "/Sessions/PlayQueue", false, false},
		{"display-settings", "POST", "/DisplayPreferences/settings", false, false},
		{"package-version", "POST", "/Packages/Installed/name", false, false},
		{"device-settings", "POST", "/Devices/Options", false, false},
		{"playbackinfo-extension", "POST", "/Items/item/PlaybackInfo", false, false},
		{"image-stream", "GET", "/Items/item/Images/Primary", true, false},
		{"image-head", "HEAD", "/Items/item/Images/Primary", true, false},
		{"unknown-api", "GET", "/Plugins/Video/Audio/operation", false, false},
		{"unknown-video-operation", "GET", "/Videos/item/UnknownOperation", false, false},
		{"unknown-video-nested", "GET", "/Videos/item/Unknown/Operation", false, false},
		{"nested-item-video", "GET", "/Items/item/Videos/stream.mp4", true, false},
		{"missing-item", "GET", "/Videos//stream.mp4", true, false},
		{"missing-resource", "GET", "/Videos/item/", true, false},
		{"stream-post", "POST", "/Videos/item/stream.mp4", true, false},
		{"stream-delete", "DELETE", "/Audio/item/stream.mp3", true, false},
	}
	for _, root := range []string{"/Videos", "/Audio"} {
		for _, method := range []string{"GET", "HEAD"} {
			for _, file := range []string{"stream", "stream.mp4", "master.m3u8", "file.mkv"} {
				cases = append(cases, endpoint{root + "-" + method + "-" + file, method, root + "/item/" + file, false, true})
			}
		}
		for _, file := range []string{"live.m3u8", "main.m3u8"} {
			cases = append(cases, endpoint{root + "-GET-" + file, "GET", root + "/item/" + file, false, true},
				endpoint{root + "-HEAD-" + file, "HEAD", root + "/item/" + file, false, false})
		}
		cases = append(cases, endpoint{root + "-proxy-hls", "GET", root + "/item/hls1/main/seg.ts", true, true},
			endpoint{root + "-proxy-head", "HEAD", root + "/item/hls1/main/seg.ts", true, true})
	}
	for _, file := range []string{"universal", "universal.mp3"} {
		for _, method := range []string{"GET", "HEAD"} {
			cases = append(cases, endpoint{"audio-" + method + "-" + file, method, "/Audio/item/" + file, false, true})
		}
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			params := queryIdentityConflicts()
			before := copyValues(params)
			resp, err := c.doRequest(context.Background(), nil, tc.method, tc.path, params, nil, nil, tc.stream)
			if err != nil {
				t.Fatal(err)
			}
			_ = resp.Body.Close()
			got := <-records
			want := infuseClientHeaderWant()
			if tc.normalize {
				want["DeviceId"] = "infuse-spoof-id"
			}
			assertClientQuery(t, got.query, want)
			for key, values := range before {
				if strings.HasPrefix(strings.ToLower(key), "x-emby-") && key != "X-Emby-DeviceId" ||
					tc.normalize && strings.EqualFold(key, "DeviceId") {
					continue
				}
				if !reflect.DeepEqual(got.query[key], values) {
					t.Fatalf("business %s changed: %v -> %v", key, values, got.query[key])
				}
			}
			if !reflect.DeepEqual(params, before) {
				t.Fatal("source query mutated")
			}
		})
	}
}

func TestOutboundClientQueryProfilesAndAbsent(t *testing.T) {
	cases := []struct {
		name string
		cfg  UpstreamConfig
		want map[string]string
	}{
		{"none", UpstreamConfig{SpoofClient: "none"}, map[string]string{
			"X-Emby-Client": "Emby Aggregator", "X-Emby-Client-Version": "1.0.0",
			"X-Emby-Device-Name": "EmbyInOne", "X-Emby-Device-Id": "emby-in-one-proxy", "DeviceId": "emby-in-one-proxy"}},
		{"infuse", UpstreamConfig{SpoofClient: "infuse"}, map[string]string{
			"X-Emby-Client": "Infuse", "X-Emby-Client-Version": "7.7.1",
			"X-Emby-Device-Name": "iPhone", "X-Emby-Device-Id": "infuse-spoof-id", "DeviceId": "infuse-spoof-id"}},
		{"custom-escaped", UpstreamConfig{SpoofClient: "custom", CustomUserAgent: "Custom/1", CustomClient: "客户端 +%,\"",
			CustomClientVersion: "v%2F+", CustomDeviceName: "设备/\\", CustomDeviceId: "fake+%2F 中文"}, map[string]string{
			"X-Emby-Client": "客户端 +%,\"", "X-Emby-Client-Version": "v%2F+", "X-Emby-Device-Name": "设备/\\",
			"X-Emby-Device-Id": "fake+%2F 中文", "DeviceId": "fake+%2F 中文"}},
		{"custom-partial", UpstreamConfig{SpoofClient: "custom", CustomClient: "Only"}, map[string]string{
			"X-Emby-Client": "Only", "X-Emby-Client-Version": "", "X-Emby-Device-Name": "", "X-Emby-Device-Id": "", "DeviceId": ""}},
		{"custom-empty", UpstreamConfig{SpoofClient: "custom"}, map[string]string{
			"X-Emby-Client": "", "X-Emby-Client-Version": "", "X-Emby-Device-Name": "", "X-Emby-Device-Id": "", "DeviceId": ""}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			records := make(chan outboundClientHeaderRecord, 1)
			stub := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				records <- recordClientHeaders(r)
				_, _ = io.WriteString(w, "{}")
			}))
			defer stub.Close()
			tc.cfg.URL = stub.URL
			c := newUpstreamClient(Config{}, tc.cfg, 0, nil)
			c.setOnline("UPSTREAM-TOKEN", "UPSTREAM-USER")
			for _, present := range []bool{true, false} {
				t.Run(fmt.Sprint(present), func(t *testing.T) {
					params := url.Values{"marker": {"unchanged"}}
					if present {
						params = queryIdentityConflicts()
					}
					before := copyValues(params)
					resp, err := c.doRequest(context.Background(), nil, http.MethodGet, "/Audio/song/universal", params, nil, nil, true)
					if err != nil {
						t.Fatal(err)
					}
					_ = resp.Body.Close()
					got := <-records
					want := tc.want
					if !present {
						want = map[string]string{"X-Emby-Client": "", "X-Emby-Client-Version": "", "X-Emby-Device-Name": "", "X-Emby-Device-Id": "", "DeviceId": ""}
					}
					assertClientQuery(t, got.query, want)
					if present {
						for key, value := range tc.want {
							if key != "DeviceId" && got.headers.Get(key) != value {
								t.Fatalf("header/query differ: %s", key)
							}
						}
					}
					if !reflect.DeepEqual(params, before) {
						t.Fatal("source query mutated")
					}
				})
			}
		})
	}
}

func TestOutboundClientQueryPassthroughWire(t *testing.T) {
	records := make(chan outboundClientHeaderRecord, 1)
	stub := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		records <- recordClientHeaders(r)
		_, _ = io.WriteString(w, "{}")
	}))
	defer stub.Close()
	c := newUpstreamClient(Config{}, UpstreamConfig{URL: stub.URL, SpoofClient: "passthrough"}, 0, nil)
	c.setOnline("UPSTREAM-TOKEN", "UPSTREAM-USER")
	params := queryIdentityConflicts()
	params.Set("api_key", "LOCAL-TOKEN")
	before := copyValues(params)
	resp, err := c.doRequest(context.Background(), nil, http.MethodGet, "/Videos/item/stream.mp4", params, nil, realClientConflictHeaders(), true)
	if err != nil {
		t.Fatal(err)
	}
	_ = resp.Body.Close()
	got := <-records
	expected := copyValues(before)
	expected.Set("api_key", "UPSTREAM-TOKEN")
	if !reflect.DeepEqual(got.query, expected) || !reflect.DeepEqual(params, before) {
		t.Fatalf("passthrough query changed: %v", got.query)
	}
}

func TestOutboundClientQueryDirectLocationPreserved(t *testing.T) {
	var mu sync.Mutex
	mediaRequests := 0
	stub := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodPost && r.URL.Path == "/Users/AuthenticateByName" {
			w.Header().Set("Content-Type", "application/json")
			_, _ = io.WriteString(w, `{"AccessToken":"UPSTREAM-TOKEN","User":{"Id":"UPSTREAM-USER"}}`)
			return
		}
		if r.URL.Path == "/Items/Counts" || r.URL.Path == "/System/Info/Public" {
			_, _ = io.WriteString(w, "{}")
			return
		}
		if r.Method == http.MethodGet && r.URL.Path == "/Users/UPSTREAM-USER/Items/media-1" {
			_, _ = io.WriteString(w, `{"Id":"media-1","MediaSources":[{"Id":"media-source"}]}`)
			return
		}
		mu.Lock()
		mediaRequests++
		mu.Unlock()
		http.NotFound(w, r)
	}))
	defer stub.Close()
	config := fmt.Sprintf("server:\n  port: 8096\n  name: Test\n  id: svr\nadmin:\n  username: admin\n  password: secret\nplayback:\n  mode: redirect\ntimeouts:\n  api: 30000\n  global: 15000\n  login: 10000\n  healthCheck: 10000\n  healthInterval: 60000\nproxies: []\nupstream:\n  - name: A\n    url: %q\n    username: u\n    password: p\n", stub.URL)
	withTempAppConfig(t, config, func(app *App, handler http.Handler) {
		token := loginToken(t, handler, "secret")
		virtualID := app.IDStore.GetOrCreateVirtualID("media-1", app.Upstream.Clients()[0].ID)
		app.IDStore.GetOrCreateVirtualID("media-source", app.Upstream.Clients()[0].ID)
		for _, root := range []string{"/Videos", "/Audio"} {
			for _, method := range []string{http.MethodGet, http.MethodHead} {
				t.Run(root+"-"+method, func(t *testing.T) {
					params := queryIdentityConflicts()
					params.Set("api_key", token)
					before := copyValues(params)
					req := httptest.NewRequest(method, root+"/"+virtualID+"/stream.mp4?"+params.Encode(), nil)
					req.Header.Set("X-Emby-Token", token)
					req.Header.Set("User-Agent", "REAL-UA")
					req.Header.Set("X-Emby-Device-Id", "REAL-LOCAL-DEVICE")
					rr := httptest.NewRecorder()
					handler.ServeHTTP(rr, req)
					if rr.Code != http.StatusFound {
						t.Fatalf("status=%d body=%s", rr.Code, rr.Body.String())
					}
					location, err := url.Parse(rr.Header().Get("Location"))
					if err != nil {
						t.Fatal(err)
					}
					expected := copyValues(before)
					expected.Set("api_key", "UPSTREAM-TOKEN")
					if location.Path != root+"/media-1/stream.mp4" || !reflect.DeepEqual(location.Query(), expected) {
						t.Fatalf("direct Location changed: %s", location)
					}
					if !reflect.DeepEqual(req.URL.Query(), before) || req.Header.Get("X-Emby-Device-Id") != "REAL-LOCAL-DEVICE" {
						t.Fatal("real inbound identity mutated")
					}
				})
			}
		}
	})
	mu.Lock()
	defer mu.Unlock()
	if mediaRequests != 0 {
		t.Fatalf("direct mode fetched video: %d", mediaRequests)
	}
}

func TestOutboundClientQueryFallbackAndIsolation(t *testing.T) {
	records := make(chan outboundClientHeaderRecord, 4)
	stub := func(status int) *httptest.Server {
		return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			records <- recordClientHeaders(r)
			w.WriteHeader(status)
		}))
	}
	first, second := stub(http.StatusServiceUnavailable), stub(http.StatusOK)
	defer first.Close()
	defer second.Close()
	clients := []*UpstreamClient{
		newUpstreamClient(Config{}, UpstreamConfig{URL: first.URL, StreamingURLs: []string{first.URL, second.URL}, SpoofClient: "infuse"}, 0, nil),
		newUpstreamClient(Config{}, UpstreamConfig{URL: first.URL, StreamingURLs: []string{first.URL, second.URL}, SpoofClient: "custom", CustomDeviceId: "B-ID", CustomClient: "B"}, 1, nil),
	}
	clients[0].setOnline("A-TOKEN", "A-USER")
	clients[1].setOnline("B-TOKEN", "B-USER")
	params := queryIdentityConflicts()
	before := copyValues(params)
	var wg sync.WaitGroup
	errors := make(chan error, 2)
	for _, c := range clients {
		wg.Add(1)
		go func(c *UpstreamClient) {
			defer wg.Done()
			resp, err := c.Stream(context.Background(), nil, nil, "/Videos/item/hls1/main/seg.ts", params)
			if resp != nil {
				if resp.StatusCode != http.StatusOK {
					err = fmt.Errorf("status %d", resp.StatusCode)
				}
				_ = resp.Body.Close()
			}
			errors <- err
		}(c)
	}
	wg.Wait()
	for i := 0; i < 2; i++ {
		if err := <-errors; err != nil {
			t.Fatal(err)
		}
	}
	counts := map[string]int{}
	for i := 0; i < 4; i++ {
		got := <-records
		id, client := "infuse-spoof-id", "Infuse"
		if got.query.Get("api_key") == "B-TOKEN" {
			id, client = "B-ID", "B"
		} else if got.query.Get("api_key") != "A-TOKEN" {
			t.Fatal("wrong credential")
		}
		assertClientQuery(t, got.query, map[string]string{"DeviceId": id, "X-Emby-Device-Id": id, "X-Emby-Client": client})
		if got.headers.Get("X-Emby-Device-Id") != id {
			t.Fatal("header/query identity mismatch")
		}
		counts[id]++
	}
	if counts["B-ID"] != 2 || counts["infuse-spoof-id"] != 2 || !reflect.DeepEqual(params, before) {
		t.Fatalf("retry/isolation failed: %v", counts)
	}
}
