package backend

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"reflect"
	"strings"
	"sync"
	"testing"
)

type outboundClientHeaderRecord struct {
	headers http.Header
	query   url.Values
	body    string
	path    string
}

func recordClientHeaders(r *http.Request) outboundClientHeaderRecord {
	raw, _ := io.ReadAll(r.Body)
	return outboundClientHeaderRecord{
		headers: r.Header.Clone(), query: r.URL.Query(), body: string(raw), path: r.URL.Path,
	}
}

func assertClientHeaders(t *testing.T, got http.Header, want map[string]string, userID, token string) {
	t.Helper()
	parsed, ok := parseAuthorizationIdentityStrict(got.Get("X-Emby-Authorization"))
	if !ok {
		t.Fatalf("invalid compound header: %q", got.Get("X-Emby-Authorization"))
	}
	for header, param := range map[string]string{
		"X-Emby-Client": "Client", "X-Emby-Client-Version": "Version",
		"X-Emby-Device-Name": "Device", "X-Emby-Device-Id": "DeviceId",
	} {
		if got.Get(header) != want[header] || parsed[param] != want[header] {
			t.Fatalf("%s/compound %s = %q/%q, want %q", header, param, got.Get(header), parsed[param], want[header])
		}
		wantCount := 1
		if want[header] == "" {
			wantCount = 0
		}
		if len(got.Values(header)) != wantCount {
			t.Fatalf("%s values = %v", header, got.Values(header))
		}
	}
	if got.Get("User-Agent") != want["User-Agent"] {
		t.Fatalf("UA = %q, want %q", got.Get("User-Agent"), want["User-Agent"])
	}
	if got.Get("X-Emby-Token") != token || parsed["UserId"] != userID || parsed["Token"] != "" {
		t.Fatalf("wrong upstream authentication: token=%q compound=%v", got.Get("X-Emby-Token"), parsed)
	}
	if got.Get("Authorization") != "" || len(got.Values("X-Emby-Authorization")) != 1 {
		t.Fatalf("duplicate authentication headers: %v", got)
	}
}

func realClientConflictHeaders() http.Header {
	return http.Header{
		"User-Agent": {"REAL-UA"}, "uSER-aGENt": {"SECOND-REAL-UA"},
		"X-Emby-Client":         {"REAL-CLIENT", "SECOND-CLIENT"},
		"x-eMbY-dEvIcE-iD":      {"LOWERCASE-REAL-ID"},
		"X-Emby-Client-Version": {"REAL-VERSION"},
		"X-Emby-Device-Name":    {"REAL-DEVICE"},
		"X-Emby-Device-Id":      {"REAL-ID"},
		"Authorization":         {compoundAuthHeader("LOCAL-USER", "LOCAL-TOKEN", "COMPOUND-CLIENT", "COMPOUND-DEVICE", "COMPOUND-ID", "COMPOUND-VERSION")},
		"x-emby-authorization":  {compoundAuthHeader("", "", "OTHER-CLIENT", "", "OTHER-ID", "")},
		"X-Emby-Token":          {"LOCAL-TOKEN"}, "Range": {"bytes=12-24"},
		"Accept": {"video/*"}, "Accept-Language": {"zh-CN"}, "Accept-Encoding": {"identity"},
	}
}

func infuseClientHeaderWant() map[string]string {
	return map[string]string{
		"User-Agent":    "Infuse/7.7.1 (iPhone; iOS 17.4.1; Scale/3.00)",
		"X-Emby-Client": "Infuse", "X-Emby-Client-Version": "7.7.1",
		"X-Emby-Device-Name": "iPhone", "X-Emby-Device-Id": "infuse-spoof-id",
	}
}

func TestOutboundClientIdentityStreamExtraHeaders(t *testing.T) {
	records := make(chan outboundClientHeaderRecord, 1)
	stub := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		records <- recordClientHeaders(r)
		w.WriteHeader(http.StatusPartialContent)
		_, _ = io.WriteString(w, "video")
	}))
	defer stub.Close()
	c := newUpstreamClient(Config{}, UpstreamConfig{URL: stub.URL, SpoofClient: "infuse", FollowRedirects: true}, 0, nil)
	c.setOnline("UPSTREAM-TOKEN", "UPSTREAM-USER")
	extra := realClientConflictHeaders()
	before := extra.Clone()
	query := url.Values{"marker": {"unchanged"}}
	ctx := &RequestContext{Headers: extra.Clone()}
	resp, err := c.Stream(context.Background(), ctx, nil, "/Videos/item/stream.mp4", query, extra)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != http.StatusPartialContent || string(raw) != "video" {
		t.Fatalf("stream changed: %d %s", resp.StatusCode, raw)
	}
	got := <-records
	assertClientHeaders(t, got.headers, infuseClientHeaderWant(), "UPSTREAM-USER", "UPSTREAM-TOKEN")
	for _, key := range []string{"Range", "Accept", "Accept-Language", "Accept-Encoding"} {
		if got.headers.Get(key) != before.Get(key) {
			t.Fatalf("%s changed", key)
		}
	}
	if !reflect.DeepEqual(extra, before) || !reflect.DeepEqual(ctx.Headers, before) || query.Get("marker") != "unchanged" {
		t.Fatal("source request state mutated")
	}
}

func TestOutboundClientIdentityHeaderProfiles(t *testing.T) {
	defaultWant := map[string]string{
		"User-Agent": "Emby Aggregator/1.0", "X-Emby-Client": "Emby Aggregator",
		"X-Emby-Client-Version": "1.0.0", "X-Emby-Device-Name": "EmbyInOne",
		"X-Emby-Device-Id": "emby-in-one-proxy",
	}
	custom := UpstreamConfig{SpoofClient: "custom", CustomUserAgent: "CUSTOM-UA",
		CustomClient: "Client\"Name, Token=\"forged", CustomClientVersion: "8.9",
		CustomDeviceName: "Device\\Name", CustomDeviceId: "fixed-custom-id"}
	customWant := map[string]string{
		"User-Agent": "CUSTOM-UA", "X-Emby-Client": custom.CustomClient,
		"X-Emby-Client-Version": "8.9", "X-Emby-Device-Name": custom.CustomDeviceName,
		"X-Emby-Device-Id": "fixed-custom-id",
	}
	cases := []struct {
		name string
		cfg  UpstreamConfig
		want map[string]string
	}{
		{"none", UpstreamConfig{SpoofClient: "none"}, defaultWant},
		{"empty-selection", UpstreamConfig{}, defaultWant},
		{"unknown-selection", UpstreamConfig{SpoofClient: "unknown"}, defaultWant},
		{"infuse", UpstreamConfig{SpoofClient: "infuse"}, infuseClientHeaderWant()},
		{"custom", custom, customWant},
		{"custom-partial", UpstreamConfig{SpoofClient: "custom", CustomClient: "PARTIAL"}, map[string]string{
			"User-Agent": "Go-http-client/1.1", "X-Emby-Client": "PARTIAL",
		}},
		{"custom-empty", UpstreamConfig{SpoofClient: "custom"}, map[string]string{"User-Agent": "Go-http-client/1.1"}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			records := make(chan outboundClientHeaderRecord, 2)
			stub := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				records <- recordClientHeaders(r)
				_, _ = io.WriteString(w, "{}")
			}))
			defer stub.Close()
			tc.cfg.URL = stub.URL
			tc.cfg.FollowRedirects = true
			c := newUpstreamClient(Config{}, tc.cfg, 0, nil)
			c.setOnline("UPSTREAM-TOKEN", "UPSTREAM-USER")
			headers := realClientConflictHeaders()
			before := headers.Clone()
			body := map[string]any{"DeviceProfile": map[string]any{"Name": "REAL-ABILITY"}, "marker": "keep"}
			bodyBefore := deepCloneMap(body)
			for _, supplied := range []http.Header{headers, nil} {
				resp, err := c.doRequest(context.Background(), nil, http.MethodPost, "/System/Info", nil, body, supplied, false)
				if err != nil {
					t.Fatal(err)
				}
				_ = resp.Body.Close()
				got := <-records
				assertClientHeaders(t, got.headers, tc.want, "UPSTREAM-USER", "UPSTREAM-TOKEN")
				var gotBody map[string]any
				if err := json.Unmarshal([]byte(got.body), &gotBody); err != nil || !reflect.DeepEqual(gotBody, bodyBefore) {
					t.Fatalf("body changed: %s %v", got.body, err)
				}
			}
			if !reflect.DeepEqual(headers, before) || !reflect.DeepEqual(body, bodyBefore) {
				t.Fatal("input changed")
			}
		})
	}
}

func TestOutboundClientIdentityBootstrapHeaders(t *testing.T) {
	for _, mode := range []string{"password", "api-key"} {
		t.Run(mode, func(t *testing.T) {
			records := make(chan outboundClientHeaderRecord, 1)
			stub := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				records <- recordClientHeaders(r)
				if mode == "api-key" {
					_, _ = io.WriteString(w, "{\"Id\":\"UPSTREAM-USER\"}")
				} else {
					_, _ = io.WriteString(w, "{\"AccessToken\":\"UPSTREAM-TOKEN\",\"User\":{\"Id\":\"UPSTREAM-USER\"}}")
				}
			}))
			defer stub.Close()
			cfg := UpstreamConfig{URL: stub.URL, Username: "name", Password: "password", SpoofClient: "infuse", FollowRedirects: true}
			wantToken := ""
			wantPath := "/Users/AuthenticateByName"
			if mode == "api-key" {
				cfg.APIKey = "API-KEY"
				wantToken = "API-KEY"
				wantPath = "/Users/Me"
			}
			c := newUpstreamClient(Config{}, cfg, 0, nil)
			c.Login(context.Background(), &RequestContext{Headers: realClientConflictHeaders()}, NewClientIdentityService())
			got := <-records
			assertClientHeaders(t, got.headers, infuseClientHeaderWant(), "", wantToken)
			if got.path != wantPath || !c.Online || c.UserID != "UPSTREAM-USER" {
				t.Fatalf("bootstrap changed: %+v online=%v uid=%q", got, c.Online, c.UserID)
			}
			if mode == "password" {
				var b map[string]any
				_ = json.Unmarshal([]byte(got.body), &b)
				if !reflect.DeepEqual(b, map[string]any{"Username": "name", "Pw": "password"}) {
					t.Fatalf("login body changed: %s", got.body)
				}
			}
		})
	}
}

func TestOutboundClientIdentityPassthroughWire(t *testing.T) {
	records := make(chan outboundClientHeaderRecord, 1)
	stub := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		records <- recordClientHeaders(r)
		_, _ = io.WriteString(w, "{}")
	}))
	defer stub.Close()
	c := newUpstreamClient(Config{}, UpstreamConfig{URL: stub.URL, SpoofClient: "passthrough", FollowRedirects: true}, 0, nil)
	c.setOnline("UPSTREAM-TOKEN", "UPSTREAM-USER")
	for _, name := range []string{"separate-headers", "compound-only"} {
		t.Run(name, func(t *testing.T) {
			live := http.Header{}
			live.Set("User-Agent", "REAL-UA")
			live.Set("X-Emby-Authorization", compoundAuthHeader("LOCAL-USER", "LOCAL-TOKEN", "REAL-CLIENT", "REAL-DEVICE", "REAL-ID", "REAL-VERSION"))
			want := map[string]string{
				"User-Agent": "REAL-UA", "X-Emby-Client": "REAL-CLIENT",
				"X-Emby-Device-Name": "REAL-DEVICE", "X-Emby-Device-Id": "REAL-ID",
				"X-Emby-Client-Version": "REAL-VERSION",
			}
			if name == "separate-headers" {
				for key, value := range want {
					live.Set(key, value)
				}
			} else {
				// Preserve the existing compound-only passthrough fallback.
				// This phase must not change mergePassthroughHeaders semantics.
				want["X-Emby-Device-Name"] = "iPhone"
				want["X-Emby-Client-Version"] = "7.7.1"
			}
			ctx := &RequestContext{Headers: live, ProxyToken: "LOCAL-TOKEN"}
			before := live.Clone()
			_, err := c.RequestJSON(context.Background(), ctx, NewClientIdentityService(), http.MethodGet, "/System/Info", nil, nil)
			if err != nil {
				t.Fatal(err)
			}
			assertClientHeaders(t, (<-records).headers, want, "UPSTREAM-USER", "UPSTREAM-TOKEN")
			if !reflect.DeepEqual(live, before) {
				t.Fatal("passthrough source changed")
			}
		})
	}
}

func TestOutboundClientIdentityStreamFallback(t *testing.T) {
	records := make(chan outboundClientHeaderRecord, 2)
	stub := func(status int) *httptest.Server {
		return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			records <- recordClientHeaders(r)
			w.WriteHeader(status)
			_, _ = io.WriteString(w, "video")
		}))
	}
	first, second := stub(http.StatusServiceUnavailable), stub(http.StatusOK)
	defer first.Close()
	defer second.Close()
	c := newUpstreamClient(Config{}, UpstreamConfig{URL: first.URL, StreamingURLs: []string{first.URL, second.URL}, SpoofClient: "infuse", FollowRedirects: true}, 0, nil)
	c.setOnline("UPSTREAM-TOKEN", "UPSTREAM-USER")
	resp, err := c.Stream(context.Background(), nil, nil, "/Videos/item/stream.mp4", nil, realClientConflictHeaders())
	if err != nil {
		t.Fatal(err)
	}
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("fallback failed: %d", resp.StatusCode)
	}
	for i := 0; i < 2; i++ {
		assertClientHeaders(t, (<-records).headers, infuseClientHeaderWant(), "UPSTREAM-USER", "UPSTREAM-TOKEN")
	}
}

func TestOutboundClientIdentityConcurrentIsolation(t *testing.T) {
	records := make(chan outboundClientHeaderRecord, 2)
	stub := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		records <- recordClientHeaders(r)
		_, _ = io.WriteString(w, "{}")
	}))
	defer stub.Close()
	clients := []*UpstreamClient{
		newUpstreamClient(Config{}, UpstreamConfig{URL: stub.URL, SpoofClient: "infuse", FollowRedirects: true}, 0, nil),
		newUpstreamClient(Config{}, UpstreamConfig{URL: stub.URL, SpoofClient: "custom", CustomUserAgent: "B-UA", CustomClient: "B", CustomDeviceId: "B-ID", FollowRedirects: true}, 1, nil),
	}
	clients[0].setOnline("A-TOKEN", "A-USER")
	clients[1].setOnline("B-TOKEN", "B-USER")
	shared := realClientConflictHeaders()
	before := shared.Clone()
	var wg sync.WaitGroup
	errs := make(chan error, 2)
	for _, c := range clients {
		wg.Add(1)
		go func(c *UpstreamClient) {
			defer wg.Done()
			resp, err := c.doRequest(context.Background(), nil, http.MethodGet, "/System/Info", nil, nil, shared, false)
			if resp != nil {
				_ = resp.Body.Close()
			}
			errs <- err
		}(c)
	}
	wg.Wait()
	for i := 0; i < 2; i++ {
		if err := <-errs; err != nil {
			t.Fatal(err)
		}
	}
	for i := 0; i < 2; i++ {
		got := <-records
		if got.headers.Get("X-Emby-Token") == "A-TOKEN" {
			assertClientHeaders(t, got.headers, infuseClientHeaderWant(), "A-USER", "A-TOKEN")
		} else {
			assertClientHeaders(t, got.headers, map[string]string{
				"User-Agent": "B-UA", "X-Emby-Client": "B", "X-Emby-Device-Id": "B-ID",
			}, "B-USER", "B-TOKEN")
		}
	}
	if !reflect.DeepEqual(shared, before) {
		t.Fatal("shared real headers mutated")
	}
}

func TestOutboundClientIdentityDirectURLPreserved(t *testing.T) {
	c := newUpstreamClient(Config{}, UpstreamConfig{URL: "http://up.test", SpoofClient: "infuse"}, 0, nil)
	c.setOnline("UPSTREAM-TOKEN", "UPSTREAM-USER")
	params := url.Values{
		"DeviceId": {"REAL-ID"}, "X-Emby-Device-Id": {"REAL-QUERY-ID"},
		"marker": {"keep"},
	}
	before := copyValues(params)
	location, err := c.BuildURL("/Videos/item/stream.mp4", params, true, nil)
	if err != nil {
		t.Fatal(err)
	}
	parsed, _ := url.Parse(location)
	q := parsed.Query()
	if q.Get("DeviceId") != "REAL-ID" || q.Get("X-Emby-Device-Id") != "REAL-QUERY-ID" ||
		q.Get("api_key") != "UPSTREAM-TOKEN" || !strings.HasPrefix(location, "http://up.test/Videos/item/stream.mp4?") {
		t.Fatalf("direct location changed: %s", location)
	}
	if !reflect.DeepEqual(params, before) {
		t.Fatal("direct source query mutated")
	}
}
