package backend

import (
	"encoding/json"
	"errors"
	"math"
	"net/url"
	"testing"
	"time"
)

func TestUnifiedTickParsingAndOverflowSafeThreshold(t *testing.T) {
	for _, tc := range []struct {
		name  string
		body  map[string]any
		state playbackTickState
		ticks int64
	}{
		{"absent", map[string]any{}, playbackTicksMissing, 0},
		{"null", map[string]any{"PositionTicks": nil}, playbackTicksNull, 0},
		{"zero", map[string]any{"PositionTicks": float64(0)}, playbackTicksValid, 0},
		{"integer", map[string]any{"PositionTicks": json.Number("9223372036854775807")}, playbackTicksValid, math.MaxInt64},
		{"negative", map[string]any{"PositionTicks": int64(-1)}, playbackTicksInvalid, 0},
		{"fraction", map[string]any{"PositionTicks": 1.5}, playbackTicksInvalid, 0},
		{"rounded JSON boundary", map[string]any{"PositionTicks": float64(1 << 53)}, playbackTicksInvalid, 0},
		{"string", map[string]any{"PositionTicks": "900"}, playbackTicksInvalid, 0},
		{"boolean", map[string]any{"PositionTicks": true}, playbackTicksInvalid, 0},
		{"nonfinite", map[string]any{"PositionTicks": math.Inf(1)}, playbackTicksInvalid, 0},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got := playbackTicksFromBody(tc.body, "PositionTicks")
			if got.State != tc.state || got.Ticks != tc.ticks {
				t.Fatalf("ticks=%+v", got)
			}
		})
	}
	for _, tc := range []struct {
		name  string
		query url.Values
		state playbackTickState
		ticks int64
	}{
		{"missing", url.Values{}, playbackTicksMissing, 0},
		{"zero", url.Values{"PositionTicks": {"0"}}, playbackTicksValid, 0},
		{"int64", url.Values{"PositionTicks": {"9223372036854775807"}}, playbackTicksValid, math.MaxInt64},
		{"duplicate", url.Values{"PositionTicks": {"10", "10"}}, playbackTicksInvalid, 0},
		{"empty", url.Values{"PositionTicks": {""}}, playbackTicksInvalid, 0},
		{"fraction", url.Values{"PositionTicks": {"1.5"}}, playbackTicksInvalid, 0},
		{"overflow", url.Values{"PositionTicks": {"9223372036854775808"}}, playbackTicksInvalid, 0},
	} {
		t.Run("query/"+tc.name, func(t *testing.T) {
			got := playbackTicksFromQuery(tc.query, "PositionTicks")
			if got.State != tc.state || got.Ticks != tc.ticks {
				t.Fatalf("query ticks=%+v", got)
			}
		})
	}
	source := playbackWatchSource{ServerID: "a", OriginalItemID: "i", MediaSourceID: "m"}
	for _, runtime := range []int64{1, 11, 101, 1000, math.MaxInt64} {
		boundary := runtime - runtime/10
		event := playbackWatchEvent{Kind: playbackWatchProgress, Source: source, Runtime: validPlaybackTicks(runtime)}
		event.Position = validPlaybackTicks(boundary - 1)
		if event.completionCandidate() {
			t.Fatalf("completed below integer boundary runtime=%d", runtime)
		}
		event.Position = validPlaybackTicks(boundary)
		if !event.completionCandidate() {
			t.Fatalf("missed integer boundary runtime=%d", runtime)
		}
	}
}

func TestUnifiedVersionQualifiedRuntimeAndFinalPosition(t *testing.T) {
	session := playbackWatchSession{ProxyUserID: "u", PlaybackDeviceID: "d", PlaySessionID: "p", Source: playbackWatchSource{ServerID: "a", OriginalItemID: "i", MediaSourceID: "m"}}
	candidate := playbackPositionCandidate{Session: session, Position: validPlaybackTicks(800)}
	for _, tc := range []struct {
		name     string
		value    playbackTickValue
		alter    func(*playbackWatchSession)
		expected playbackTickValue
	}{
		{"missing", playbackTickValue{State: playbackTicksMissing}, nil, validPlaybackTicks(800)},
		{"null", playbackTickValue{State: playbackTicksNull}, nil, validPlaybackTicks(800)},
		{"zero", validPlaybackTicks(0), nil, validPlaybackTicks(0)},
		{"invalid", playbackTickValue{State: playbackTicksInvalid}, nil, playbackTickValue{State: playbackTicksInvalid}},
		{"other device", playbackTickValue{}, func(s *playbackWatchSession) { s.PlaybackDeviceID = "other" }, playbackTickValue{}},
		{"other session", playbackTickValue{}, func(s *playbackWatchSession) { s.PlaySessionID = "other" }, playbackTickValue{}},
		{"other version", playbackTickValue{}, func(s *playbackWatchSession) { s.Source.MediaSourceID = "other" }, playbackTickValue{}},
		{"other source", playbackTickValue{}, func(s *playbackWatchSession) { s.Source.ServerID = "b" }, playbackTickValue{}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s := session
			if tc.alter != nil {
				tc.alter(&s)
			}
			got := resolvePlaybackPosition(playbackWatchStopped, tc.value, s, candidate)
			if got != tc.expected {
				t.Fatalf("position=%+v", got)
			}
		})
	}
	runtime := playbackRuntimeCandidate{Source: session.Source, Ticks: 1000}
	other := session.Source
	other.MediaSourceID = "other"
	if got := resolvePlaybackRuntime(other, playbackTickValue{}, runtime); got.valid() {
		t.Fatalf("borrowed another version: %+v", got)
	}
	other = session.Source
	other.ServerID = "b"
	if got := resolvePlaybackRuntime(other, playbackTickValue{}, runtime); got.valid() {
		t.Fatalf("borrowed another source: %+v", got)
	}
	omitted := session.Source
	omitted.MediaSourceID = ""
	if resolvePlaybackRuntime(omitted, playbackTickValue{}, runtime).valid() {
		t.Fatal("omitted version guessed without unique proof")
	}
	runtime.SingleMediaSource = true
	if got := resolvePlaybackRuntime(omitted, playbackTickValue{}, runtime); !got.valid() || got.Ticks != 1000 {
		t.Fatalf("single-version proof=%+v", got)
	}
}

func TestUnifiedOrderingBackwardSeekFailedStartAndTerminalWriteFailure(t *testing.T) {
	cache := &playbackWatchCache{}
	key := playbackWatchCacheKey{VirtualItemID: "film", Session: playbackWatchSession{ProxyUserID: "u", PlaybackDeviceID: "d", PlaySessionID: "p-a", Source: playbackWatchSource{ServerID: "a", OriginalItemID: "i-a", MediaSourceID: "m-a"}}}
	started := cache.admit(key, playbackWatchStarted)
	if !cache.confirmStarted(started, key.Session) {
		t.Fatal("start owner missing")
	}
	position := int64(0)
	writes := 0
	commit := func(admission *playbackWatchAdmission, event playbackWatchEvent, session playbackWatchSession) error {
		return cache.commitAdmitted(admission, WatchProgress{ItemType: "Movie"}, event, session, func() error { position = event.Position.Ticks; writes++; return nil })
	}
	event := playbackWatchEvent{Kind: playbackWatchProgress, Source: key.Session.Source, Position: validPlaybackTicks(200)}
	old, current := cache.admit(key, playbackWatchProgress), cache.admit(key, playbackWatchProgress)
	if err := commit(current, event, key.Session); err != nil {
		t.Fatal(err)
	}
	event.Position = validPlaybackTicks(800)
	if err := commit(old, event, key.Session); err != nil {
		t.Fatal(err)
	}
	if position != 200 || writes != 1 {
		t.Fatalf("late progress overwrote newer state position=%d writes=%d", position, writes)
	}
	event.Position = validPlaybackTicks(100)
	if err := commit(cache.admit(key, playbackWatchProgress), event, key.Session); err != nil {
		t.Fatal(err)
	}
	if position != 100 {
		t.Fatal("backward seek forced maximum position")
	}
	other := key
	other.Session.PlaySessionID = "p-b"
	other.Session.Source = playbackWatchSource{ServerID: "b", OriginalItemID: "i-b", MediaSourceID: "m-b"}
	failed := cache.admit(other, playbackWatchStarted)
	failedEvent := playbackWatchEvent{Kind: playbackWatchStarted, Source: other.Session.Source, Failed: true}
	if err := commit(failed, failedEvent, other.Session); err != nil {
		t.Fatal(err)
	}
	if writes != 2 {
		t.Fatal("failed Started wrote progress")
	}
	event.Position = validPlaybackTicks(500)
	if err := commit(cache.admit(key, playbackWatchProgress), event, key.Session); err != nil {
		t.Fatal(err)
	}
	if position != 500 {
		t.Fatal("failed B Started displaced owner A")
	}
	terminal := cache.admit(key, playbackWatchStopped)
	event.Kind = playbackWatchStopped
	failure := errors.New("isolated watch persistence failure")
	if err := cache.commitAdmitted(terminal, WatchProgress{}, event, key.Session, func() error { return failure }); !errors.Is(err, failure) {
		t.Fatalf("write error=%v", err)
	}
	if cache.admit(key, playbackWatchProgress) != nil {
		t.Fatal("failed terminal persistence left old owner open")
	}
	restarted := cache.admit(key, playbackWatchStarted)
	if !cache.confirmStarted(restarted, key.Session) || cache.admit(key, playbackWatchProgress) == nil {
		t.Fatal("new successful Started did not recover owner")
	}
}

func TestUnifiedEvictionAndManualResetInvalidateAdmissions(t *testing.T) {
	for _, mode := range []string{"manual reset", "expiry"} {
		t.Run(mode, func(t *testing.T) {
			cache := &playbackWatchCache{}
			key := playbackWatchCacheKey{VirtualItemID: "film", Session: playbackWatchSession{ProxyUserID: "u", PlaybackDeviceID: "d", PlaySessionID: "p", Source: playbackWatchSource{ServerID: "a", OriginalItemID: "i", MediaSourceID: "m"}}}
			if !cache.confirmStarted(cache.admit(key, playbackWatchStarted), key.Session) {
				t.Fatal("owner missing")
			}
			admission := cache.admit(key, playbackWatchProgress)
			cache.mu.Lock()
			if mode == "manual reset" {
				cache.invalidateShared("u", "film")
			} else {
				cache.prune(time.Now().Add(activeStreamTTL + time.Second))
			}
			cache.mu.Unlock()
			writes := 0
			event := playbackWatchEvent{Kind: playbackWatchProgress, Source: key.Session.Source, Position: validPlaybackTicks(900)}
			if err := cache.commitAdmitted(admission, WatchProgress{}, event, key.Session, func() error { writes++; return nil }); err != nil {
				t.Fatal(err)
			}
			if writes != 0 || cache.admit(key, playbackWatchProgress) != nil {
				t.Fatal("stale admission recreated writer")
			}
		})
	}
}
