package backend

import (
	"bufio"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"sync"
	"testing"
)

func TestSampleQuerySummaryRedactsSensitiveValues(t *testing.T) {
	values := url.Values{
		"SearchTerm":    {"Secret Movie"},
		"Ids":           {"alpha,beta,gamma"},
		"ParentId":      {"parent-secret"},
		"MediaSourceId": {"source-secret"},
		"Limit":         {"100"},
		"StartIndex":    {"200"},
		"Fields":        {"ProviderIds,MediaSources"},
	}
	got := sampleQuerySummary(values)
	if got["search_term_present"] != true || got["search_term_length"] != 12 {
		t.Fatalf("search term summary = %#v", got)
	}
	if got["ids_count"] != 3 || got["parentid_present"] != true || got["mediasourceid_present"] != true {
		t.Fatalf("sensitive query summary = %#v", got)
	}
	encoded, err := json.Marshal(got)
	if err != nil {
		t.Fatal(err)
	}
	text := string(encoded)
	for _, secret := range []string{"Secret Movie", "alpha", "parent-secret", "source-secret"} {
		if strings.Contains(text, secret) {
			t.Fatalf("sample query leaked %q in %s", secret, text)
		}
	}
	if got["limit"] != 100 || got["startindex"] != 200 {
		t.Fatalf("paging summary = %#v", got)
	}
}

func TestSampleCollectorLifecycleAndEvents(t *testing.T) {
	collector := newSampleCollector(t.TempDir())
	status, err := collector.Start("test baseline")
	if err != nil {
		t.Fatal(err)
	}
	if !status.Active || status.SessionID == "" {
		t.Fatalf("start status = %+v", status)
	}

	inbound := collector.BeginInbound("trace-1", "test", http.MethodGet, "/emby/Users/user-secret/Items", url.Values{"Limit": {"50"}})
	if inbound == nil {
		t.Fatal("inbound span not created")
	}
	outbound := collector.BeginOutbound("trace-1", sampleSourceClient, "test", "终点站", http.MethodGet,
		"/Users/upstream-user/Items", url.Values{"Limit": {"50"}, "SearchTerm": {"private title"}}, false)
	if outbound == nil {
		t.Fatal("outbound span not created")
	}
	outbound.SetReturnedItems(50)
	body := &sampleBodyCapture{
		ReadCloser: io.NopCloser(strings.NewReader("abc")),
		span: outbound, status: http.StatusOK, expected: 123,
	}
	if _, err := io.ReadAll(body); err != nil {
		t.Fatal(err)
	}
	if err := body.Close(); err != nil {
		t.Fatal(err)
	}
	inbound.Finish(http.StatusOK, 456, "GET /Users/{userId}/Items")
	final := collector.Stop()
	if final.Active || final.InboundPeak != 1 || final.OutboundPeak != 1 || final.OutboundPeakByUpstream["终点站"] != 1 {
		t.Fatalf("final status = %+v", final)
	}
	if final.Bytes <= 0 || final.Truncated {
		t.Fatalf("capture size status = %+v", final)
	}

	path := filepath.Join(collector.dataDir, "samples", final.File)
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if info.Size() != final.Bytes {
		t.Fatalf("capture file size=%d status bytes=%d", info.Size(), final.Bytes)
	}
	file, err := os.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer file.Close()

	var events []sampleEvent
	scanner := bufio.NewScanner(file)
	for scanner.Scan() {
		var event sampleEvent
		if err := json.Unmarshal(scanner.Bytes(), &event); err != nil {
			t.Fatal(err)
		}
		events = append(events, event)
	}
	if err := scanner.Err(); err != nil {
		t.Fatal(err)
	}
	if len(events) != 4 {
		t.Fatalf("events = %d, want 4: %#v", len(events), events)
	}
	var gotInbound, gotOutbound *sampleEvent
	for i := range events {
		switch events[i].Event {
		case "inbound":
			gotInbound = &events[i]
		case "outbound":
			gotOutbound = &events[i]
		}
	}
	if gotInbound == nil || gotInbound.TraceID != "trace-1" || gotInbound.User != "test" ||
		gotInbound.ResponseBytes != 456 || gotInbound.Path != "/Users/{userId}/Items" {
		t.Fatalf("inbound event = %#v", gotInbound)
	}
	if gotOutbound == nil || gotOutbound.TraceID != "trace-1" || gotOutbound.Upstream != "终点站" ||
		gotOutbound.ResponseBytes != 123 || gotOutbound.ReturnedItems == nil || *gotOutbound.ReturnedItems != 50 {
		t.Fatalf("outbound event = %#v", gotOutbound)
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(raw), "private title") || strings.Contains(string(raw), "upstream-user") {
		t.Fatalf("sample file leaked raw activity identifiers: %s", raw)
	}
}

func TestSampleMergeQuality(t *testing.T) {
	items := []map[string]any{
		{
			"Id": "1", "Type": "Movie",
			"ProviderIds": map[string]any{"Tmdb": "1", "Imdb": "tt1"},
			"MediaSources": []any{map[string]any{"Id": "source"}},
		},
		{
			"Id": "2", "Type": "Series",
			"ProviderIds": map[string]any{"Tvdb": "2"},
		},
		{"Id": "3", "Type": "Movie"},
	}
	got := sampleMergeQuality(items)
	if got["encountered_items"] != 3 || got["type_present"] != 3 || got["provider_any"] != 2 ||
		got["provider_tmdb"] != 1 || got["provider_imdb"] != 1 || got["provider_tvdb"] != 1 ||
		got["media_sources_present"] != 1 || got["movie_episode_items"] != 2 {
		t.Fatalf("quality = %#v", got)
	}
}

func TestSamplePathClass(t *testing.T) {
	cases := map[string]string{
		"/Items/abc/PlaybackInfo": "/Items/{itemId}/PlaybackInfo",
		"/Users/123/Items":        "/Users/{userId}/Items",
		"/Shows/xyz/Episodes":     "/Shows/{seriesId}/Episodes",
		"/Search/Hints":           "/Search/Hints",
		"/emby/Items/0123456789abcdef0123456789abcdef/PlaybackInfo": "/Items/{itemId}/PlaybackInfo",
		"/api/danmu/0123456789abcdef0123456789abcdef": "/api/danmu/{id}",
		"/Videos/movie-secret/ms-readable/Subtitles/0/Stream.srt": "/Videos/{itemId}/{mediaSourceId}/Subtitles/0/Stream.srt",
		"/Audio/audio-secret/ms-readable/Attachments/2": "/Audio/{itemId}/{mediaSourceId}/Attachments/2",
	}
	for input, want := range cases {
		if got := samplePathClass(input); got != want {
			t.Fatalf("samplePathClass(%q) = %q, want %q", input, got, want)
		}
	}
}


func TestSampleCollectorTargetUserIsolation(t *testing.T) {
	collector := newSampleCollector(t.TempDir())
	status, err := collector.Start("targeted", "test")
	if err != nil {
		t.Fatal(err)
	}
	if status.TargetUser != "test" {
		t.Fatalf("target user = %q", status.TargetUser)
	}
	if span := collector.BeginInbound("other-trace", "other", http.MethodGet, "/Items", nil); span != nil {
		t.Fatal("non-target inbound request was sampled")
	}
	if span := collector.BeginOutbound("", sampleSourceClient, "other", "终点站", http.MethodGet, "/Items", nil, false); span != nil {
		t.Fatal("non-target outbound request was sampled")
	}
	background := collector.BeginOutbound("", sampleSourceMediaCounts, "", "终点站", http.MethodGet, "/Items/Counts", nil, false)
	if background == nil {
		t.Fatal("background request should remain sampled for baseline subtraction")
	}
	background.Finish(http.StatusOK, 10, nil)
	target := collector.BeginInbound("target-trace", "test", http.MethodGet, "/Items", nil)
	if target == nil {
		t.Fatal("target request was not sampled")
	}
	target.Finish(http.StatusOK, 20, "")
	final := collector.Stop()
	if final.InboundPeak != 1 || final.OutboundPeak != 1 {
		t.Fatalf("targeted peaks = %+v", final)
	}
}


func TestAdminSampleCaptureControlsValidateTargetUser(t *testing.T) {
	withTempApp(t, func(app *App, handler http.Handler) {
		adminToken := loginToken(t, handler, "secret")
		create := doJSONRequest(t, handler, http.MethodPost, "/admin/api/users", map[string]any{
			"username": "test",
			"password": "test-password",
			"enabled": true,
			"allowedServers": []string{},
		}, adminToken)
		if create.Code != http.StatusCreated && create.Code != http.StatusOK {
			t.Fatalf("create sample user: status=%d body=%s", create.Code, create.Body.String())
		}

		bad := doJSONRequest(t, handler, http.MethodPost, "/admin/api/samples/start", map[string]any{
			"label": "bad-target", "user": "missing-user",
		}, adminToken)
		if bad.Code != http.StatusBadRequest {
			t.Fatalf("missing target start status=%d body=%s", bad.Code, bad.Body.String())
		}

		start := doJSONRequest(t, handler, http.MethodPost, "/admin/api/samples/start", map[string]any{
			"label": "test-target", "user": "test",
		}, adminToken)
		if start.Code != http.StatusOK {
			t.Fatalf("sample start status=%d body=%s", start.Code, start.Body.String())
		}
		var started sampleStatus
		if err := json.Unmarshal(start.Body.Bytes(), &started); err != nil {
			t.Fatal(err)
		}
		if !started.Active || started.TargetUser != "test" || started.File == "" {
			t.Fatalf("start status=%+v", started)
		}

		statusRR := doJSONRequest(t, handler, http.MethodGet, "/admin/api/samples/status", nil, adminToken)
		if statusRR.Code != http.StatusOK {
			t.Fatalf("sample status=%d body=%s", statusRR.Code, statusRR.Body.String())
		}
		var status sampleStatus
		if err := json.Unmarshal(statusRR.Body.Bytes(), &status); err != nil {
			t.Fatal(err)
		}
		if !status.Active || status.SessionID != started.SessionID || status.TargetUser != "test" {
			t.Fatalf("status=%+v started=%+v", status, started)
		}

		stop := doJSONRequest(t, handler, http.MethodPost, "/admin/api/samples/stop", nil, adminToken)
		if stop.Code != http.StatusOK {
			t.Fatalf("sample stop=%d body=%s", stop.Code, stop.Body.String())
		}
		var stopped sampleStatus
		if err := json.Unmarshal(stop.Body.Bytes(), &stopped); err != nil {
			t.Fatal(err)
		}
		if stopped.Active {
			t.Fatalf("stop status=%+v", stopped)
		}
	})
}


type phase1BRecordedCall struct {
	Method string
	Path   string
	Query  string
}

type phase1BUpstreamRecorder struct {
	mu    sync.Mutex
	calls []phase1BRecordedCall
}

func (r *phase1BUpstreamRecorder) add(req *http.Request) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.calls = append(r.calls, phase1BRecordedCall{
		Method: req.Method,
		Path:   req.URL.Path,
		Query:  req.URL.Query().Encode(),
	})
}

func (r *phase1BUpstreamRecorder) itemListCalls() []phase1BRecordedCall {
	r.mu.Lock()
	defer r.mu.Unlock()
	var out []phase1BRecordedCall
	for _, call := range r.calls {
		if call.Path == "/Users/user-a/Items" {
			out = append(out, call)
		}
	}
	return out
}

func phase1BListUpstream(t *testing.T, recorder *phase1BUpstreamRecorder) *httptest.Server {
	t.Helper()
	movies := []any{
		map[string]any{
			"Id": "movie-a", "Type": "Movie", "Name": "Sensitive Title One", "ProductionYear": 2024,
			"ProviderIds": map[string]any{"Tmdb": "1001"},
			"MediaSources": []any{map[string]any{"Id": "ms-a", "ItemId": "movie-a", "Container": "mkv"}},
		},
		map[string]any{
			"Id": "movie-b", "Type": "Movie", "Name": "Sensitive Title Two", "ProductionYear": 2025,
			"ProviderIds": map[string]any{"Tmdb": "1002"},
			"MediaSources": []any{map[string]any{"Id": "ms-b", "ItemId": "movie-b", "Container": "mp4"}},
		},
	}
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		recorder.add(r)
		w.Header().Set("Content-Type", "application/json")
		switch {
		case r.Method == http.MethodPost && r.URL.Path == "/Users/AuthenticateByName":
			_ = json.NewEncoder(w).Encode(map[string]any{
				"AccessToken": "phase1b-upstream-token",
				"User": map[string]any{"Id": "user-a"},
			})
		case r.Method == http.MethodGet && r.URL.Path == "/System/Info/Public":
			_ = json.NewEncoder(w).Encode(map[string]any{"Id": "phase1b-source"})
		case r.Method == http.MethodGet && r.URL.Path == "/Items/Counts":
			_ = json.NewEncoder(w).Encode(map[string]any{"MovieCount": 2, "SeriesCount": 0, "EpisodeCount": 0})
		case r.Method == http.MethodGet && r.URL.Path == "/Users/user-a/Items":
			_ = json.NewEncoder(w).Encode(map[string]any{
				"Items": movies, "TotalRecordCount": 2, "StartIndex": 0,
			})
		case r.Method == http.MethodGet && r.URL.Path == "/Videos/probe":
			http.NotFound(w, r)
		default:
			http.NotFound(w, r)
		}
	}))
}

func phase1BConfig(upstreamURL string) string {
	return fmt.Sprintf(`server:
  port: 8096
  name: "Phase1B"
  id: "phase1b-server"
admin:
  username: "admin"
  password: "secret"
playback:
  mode: "proxy"
timeouts:
  api: 30000
  global: 15000
  login: 10000
  healthCheck: 10000
  healthInterval: 600000
proxies: []
upstream:
  - id: "server-a"
    name: "A"
    url: %q
    username: "u1"
    password: "p1"
    maxConcurrent: 0
`, upstreamURL)
}

func phase1BCreateUser(t *testing.T, app *App, handler http.Handler, username, password string) (string, string) {
	t.Helper()
	adminToken := loginTokenAs(t, handler, "admin", "secret")
	create := doJSONRequest(t, handler, http.MethodPost, "/admin/api/users", map[string]any{
		"username": username,
		"password": password,
		"allowedServers": []string{"server-a"},
	}, adminToken)
	if create.Code != http.StatusCreated {
		t.Fatalf("create %s user: status=%d body=%s", username, create.Code, create.Body.String())
	}
	token := loginTokenAs(t, handler, username, password)
	info := app.Auth.ValidateToken(token)
	if info == nil {
		t.Fatalf("%s token missing", username)
	}
	return token, info.UserID
}

func phase1BCreateTestUser(t *testing.T, app *App, handler http.Handler) (string, string) {
	return phase1BCreateUser(t, app, handler, "test", "phase1b-password")
}

func phase1BUserItemsRequest(t *testing.T, handler http.Handler, token, userID string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(http.MethodGet,
		"/Users/"+userID+"/Items?Limit=2&SearchTerm="+url.QueryEscape("Sensitive Title"), nil)
	req.Header.Set("X-Emby-Token", token)
	req.Header.Set("X-Emby-Device-Id", "phase1b-device")
	rr := httptest.NewRecorder()
	handler.ServeHTTP(rr, req)
	if rr.Code != http.StatusOK {
		t.Fatalf("user items status=%d body=%s", rr.Code, rr.Body.String())
	}
	return rr
}

func readSampleEvents(t *testing.T, path string) []sampleEvent {
	t.Helper()
	file, err := os.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer file.Close()
	var events []sampleEvent
	scanner := bufio.NewScanner(file)
	for scanner.Scan() {
		var event sampleEvent
		if err := json.Unmarshal(scanner.Bytes(), &event); err != nil {
			t.Fatal(err)
		}
		events = append(events, event)
	}
	if err := scanner.Err(); err != nil {
		t.Fatal(err)
	}
	return events
}

func TestPhase1BCollectorOffCreatesNoSampleArtifacts(t *testing.T) {
	dir := t.TempDir()
	collector := newSampleCollector(dir)
	if collector.Enabled() {
		t.Fatal("collector unexpectedly enabled")
	}
	if span := collector.BeginInbound("trace", "test", http.MethodGet, "/Items", nil); span != nil {
		t.Fatal("disabled collector created inbound span")
	}
	if span := collector.BeginOutbound("trace", sampleSourceClient, "test", "A", http.MethodGet, "/Items", nil, false); span != nil {
		t.Fatal("disabled collector created outbound span")
	}
	if _, err := os.Stat(filepath.Join(dir, "samples")); !os.IsNotExist(err) {
		t.Fatalf("disabled collector created sample directory: err=%v", err)
	}
	allocs := testing.AllocsPerRun(1000, func() {
		if collector.BeginInbound("trace", "test", http.MethodGet, "/Items", nil) != nil {
			panic("disabled collector returned a span")
		}
	})
	if allocs != 0 {
		t.Fatalf("disabled BeginInbound allocations = %v, want 0", allocs)
	}
}

func TestPhase1BCollectorDoesNotChangeUpstreamRequestBehavior(t *testing.T) {
	recorder := &phase1BUpstreamRecorder{}
	upstream := phase1BListUpstream(t, recorder)
	defer upstream.Close()

	withTempAppConfig(t, phase1BConfig(upstream.URL), func(app *App, handler http.Handler) {
		token, userID := phase1BCreateTestUser(t, app, handler)
		otherToken, otherUserID := phase1BCreateUser(t, app, handler, "other", "phase1b-other-password")

		beforeOff := recorder.itemListCalls()
		off := phase1BUserItemsRequest(t, handler, token, userID)
		afterOff := recorder.itemListCalls()
		offCalls := append([]phase1BRecordedCall(nil), afterOff[len(beforeOff):]...)
		if len(offCalls) != 1 {
			t.Fatalf("collector OFF upstream item-list calls=%d calls=%+v", len(offCalls), offCalls)
		}

		status, err := app.SampleCollector.Start("phase1b-behavior", "test")
		if err != nil {
			t.Fatal(err)
		}
		if !status.Active || status.TargetUser != "test" {
			t.Fatalf("sample start=%+v", status)
		}
		beforeOn := recorder.itemListCalls()
		on := phase1BUserItemsRequest(t, handler, token, userID)
		afterOn := recorder.itemListCalls()
		onCalls := append([]phase1BRecordedCall(nil), afterOn[len(beforeOn):]...)

		// A real request from another regular user must still reach the upstream,
		// but it must not enter a capture targeted at test.
		_ = phase1BUserItemsRequest(t, handler, otherToken, otherUserID)
		stopped := app.SampleCollector.Stop()

		if len(onCalls) != 1 {
			t.Fatalf("collector ON upstream item-list calls=%d calls=%+v", len(onCalls), onCalls)
		}
		if !reflect.DeepEqual(offCalls, onCalls) {
			t.Fatalf("collector changed upstream request behavior\nOFF=%+v\nON=%+v", offCalls, onCalls)
		}
		if off.Body.String() != on.Body.String() {
			t.Fatalf("collector changed client response\nOFF=%s\nON=%s", off.Body.String(), on.Body.String())
		}

		samplePath := filepath.Join(app.SampleCollector.dataDir, "samples", stopped.File)
		events := readSampleEvents(t, samplePath)
		var inbound *sampleEvent
		var outbound *sampleEvent
		var quality *sampleEvent
		for i := range events {
			event := &events[i]
			if event.User == "other" {
				t.Fatalf("non-target user leaked into capture: %+v", event)
			}
			if event.User != "test" {
				continue
			}
			switch event.Event {
			case "inbound":
				if event.Path == "/Users/{userId}/Items" {
					inbound = event
				}
			case "outbound":
				if event.Path == "/Users/{userId}/Items" {
					outbound = event
				}
			case "merge_quality":
				quality = event
			}
		}
		if inbound == nil || outbound == nil {
			t.Fatalf("missing linked sample events: %+v", events)
		}
		if inbound.TraceID == "" || inbound.TraceID != outbound.TraceID {
			t.Fatalf("trace mismatch inbound=%+v outbound=%+v", inbound, outbound)
		}
		if outbound.Source != sampleSourceClient || outbound.Upstream != "A" ||
			outbound.Status != http.StatusOK || outbound.ResponseBytes <= 0 ||
			outbound.ReturnedItems == nil || *outbound.ReturnedItems != 2 ||
			outbound.Concurrency < 1 || outbound.UpstreamConcurrency < 1 {
			t.Fatalf("outbound metrics invalid: %+v", outbound)
		}
		if quality == nil || quality.TraceID != inbound.TraceID ||
			quality.Stats["encountered_items"] != 2 ||
			quality.Stats["provider_any"] != 2 ||
			quality.Stats["media_sources_present"] != 2 ||
			quality.Stats["need_metadata_hydrate"] != 0 ||
			quality.Stats["need_parent_hydrate"] != 0 {
			t.Fatalf("merge quality invalid: %+v", quality)
		}

		raw, err := os.ReadFile(samplePath)
		if err != nil {
			t.Fatal(err)
		}
		text := string(raw)
		for _, secret := range []string{
			"Sensitive Title",
			"phase1b-upstream-token",
			"phase1b-password",
			"user-a",
			"movie-a",
			"movie-b",
			"ms-a",
			"ms-b",
		} {
			if strings.Contains(text, secret) {
				t.Fatalf("sample file leaked %q: %s", secret, text)
			}
		}
	})
}


func phase1BHydrationUpstream(t *testing.T) *httptest.Server {
	t.Helper()
	partialEpisode := map[string]any{
		"Id": "episode-1", "Type": "Episode", "Name": "Episode One", "SeriesId": "series-1",
	}
	fullEpisode := map[string]any{
		"Id": "episode-1", "Type": "Episode", "Name": "Episode One", "SeriesId": "series-1",
		"ProviderIds": map[string]any{"Tvdb": "2001"},
		"MediaSources": []any{map[string]any{"Id": "episode-source-1", "ItemId": "episode-1", "Container": "mkv"}},
	}
	fullSeries := map[string]any{
		"Id": "series-1", "Type": "Series", "Name": "Series One", "ProductionYear": 2024,
		"ProviderIds": map[string]any{"Tvdb": "3001"},
	}
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch {
		case r.Method == http.MethodPost && r.URL.Path == "/Users/AuthenticateByName":
			_ = json.NewEncoder(w).Encode(map[string]any{
				"AccessToken": "phase1b-hydration-upstream-token",
				"User": map[string]any{"Id": "user-a"},
			})
		case r.Method == http.MethodGet && r.URL.Path == "/System/Info/Public":
			_ = json.NewEncoder(w).Encode(map[string]any{"Id": "phase1b-hydration-source"})
		case r.Method == http.MethodGet && r.URL.Path == "/Items/Counts":
			_ = json.NewEncoder(w).Encode(map[string]any{"MovieCount": 0, "SeriesCount": 1, "EpisodeCount": 1})
		case r.Method == http.MethodGet && r.URL.Path == "/Users/user-a/Items":
			_ = json.NewEncoder(w).Encode(map[string]any{
				"Items": []any{partialEpisode}, "TotalRecordCount": 1, "StartIndex": 0,
			})
		case r.Method == http.MethodGet && r.URL.Path == "/Items":
			switch r.URL.Query().Get("Ids") {
			case "episode-1":
				_ = json.NewEncoder(w).Encode(map[string]any{"Items": []any{fullEpisode}, "TotalRecordCount": 1})
			case "series-1":
				_ = json.NewEncoder(w).Encode(map[string]any{"Items": []any{fullSeries}, "TotalRecordCount": 1})
			default:
				_ = json.NewEncoder(w).Encode(map[string]any{"Items": []any{}, "TotalRecordCount": 0})
			}
		case r.Method == http.MethodGet && r.URL.Path == "/Videos/probe":
			http.NotFound(w, r)
		default:
			http.NotFound(w, r)
		}
	}))
}

func TestPhase1BHydrateAndParentHydrateAreClassifiedAndLinked(t *testing.T) {
	upstream := phase1BHydrationUpstream(t)
	defer upstream.Close()

	withTempAppConfig(t, phase1BConfig(upstream.URL), func(app *App, handler http.Handler) {
		token, userID := phase1BCreateTestUser(t, app, handler)
		status, err := app.SampleCollector.Start("phase1b-hydration", "test")
		if err != nil {
			t.Fatal(err)
		}
		if !status.Active {
			t.Fatalf("sample start=%+v", status)
		}
		_ = phase1BUserItemsRequest(t, handler, token, userID)
		stopped := app.SampleCollector.Stop()
		samplePath := filepath.Join(app.SampleCollector.dataDir, "samples", stopped.File)
		events := readSampleEvents(t, samplePath)

		var traceID string
		for i := range events {
			event := &events[i]
			if event.Event == "inbound" && event.User == "test" && event.Path == "/Users/{userId}/Items" {
				traceID = event.TraceID
				break
			}
		}
		if traceID == "" {
			t.Fatalf("missing inbound trace: %+v", events)
		}

		sources := map[string]*sampleEvent{}
		var quality *sampleEvent
		for i := range events {
			event := &events[i]
			if event.TraceID != traceID {
				continue
			}
			if event.Event == "outbound" {
				sources[event.Source] = event
			}
			if event.Event == "merge_quality" {
				quality = event
			}
		}
		for _, source := range []string{sampleSourceClient, sampleSourceHydrate, sampleSourceParentHydrate} {
			if sources[source] == nil {
				t.Fatalf("missing %s event for trace %s: %+v", source, traceID, events)
			}
		}
		if sources[sampleSourceHydrate].Path != "/Items" ||
			fmt.Sprint(sources[sampleSourceHydrate].Query["ids_count"]) != "1" ||
			sources[sampleSourceHydrate].ReturnedItems == nil ||
			*sources[sampleSourceHydrate].ReturnedItems != 1 {
			t.Fatalf("hydrate event=%+v", sources[sampleSourceHydrate])
		}
		if sources[sampleSourceParentHydrate].Path != "/Items" ||
			fmt.Sprint(sources[sampleSourceParentHydrate].Query["ids_count"]) != "1" ||
			sources[sampleSourceParentHydrate].ReturnedItems == nil ||
			*sources[sampleSourceParentHydrate].ReturnedItems != 1 {
			t.Fatalf("parent hydrate event=%+v", sources[sampleSourceParentHydrate])
		}
		if quality == nil ||
			quality.Stats["encountered_items"] != 1 ||
			quality.Stats["need_metadata_hydrate"] != 1 ||
			quality.Stats["metadata_hydrate_batches"] != 1 ||
			quality.Stats["need_parent_hydrate"] != 1 ||
			quality.Stats["parent_hydrate_batches"] != 1 {
			t.Fatalf("hydrate quality=%+v", quality)
		}

		raw, err := os.ReadFile(samplePath)
		if err != nil {
			t.Fatal(err)
		}
		text := string(raw)
		for _, secret := range []string{
			"episode-1", "series-1", "episode-source-1", "phase1b-hydration-upstream-token",
		} {
			if strings.Contains(text, secret) {
				t.Fatalf("hydrate sample leaked %q: %s", secret, text)
			}
		}
	})
}
