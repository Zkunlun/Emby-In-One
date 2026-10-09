package backend

import (
	"bufio"
	"encoding/json"
	"io"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"
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

	path := filepath.Join(collector.dataDir, "samples", final.File)
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
		gotInbound.ResponseBytes != 456 || gotInbound.Path != "GET /Users/{userId}/Items" {
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
	}
	for input, want := range cases {
		if got := samplePathClass(input); got != want {
			t.Fatalf("samplePathClass(%q) = %q, want %q", input, got, want)
		}
	}
}
