package backend

import (
	"context"
	"encoding/json"
	"net/http"
	"strconv"
	"sync/atomic"
	"testing"
	"time"
)

// A failed initial background round must be scheduled again with bounded
// backoff. The authenticated HTTP reader must never start the retry itself.
func TestPhase3CountsFailedInitialRoundSchedulesBoundedRetry(t *testing.T) {
	now := time.Unix(1700000000, 0)
	calls := 0
	cache := countsTestCache(t, func() time.Time { return now }, countsTestCollectorFunc(func(context.Context, countsSourceSpec) countsRoundResult {
		calls++
		if calls == 1 {
			return countsRoundResult{Class: countsTransport, CountsCalls: 1}
		}
		return countsRoundResult{Success: true, Value: mediaCounts{7, 8, 9}}
	}))
	client := countsTestClient("a")
	cache.syncSource(cache.prepareSource(client, nil, ""))
	if result, owned := cache.refresh(context.Background(), "a"); !owned || result.Success {
		t.Fatalf("initial round expected to fail: %+v owned=%v", result, owned)
	}
	state := cache.read("a")
	if state.HasSnapshot || !state.Pending || !state.NextAllowedAt.After(now) {
		t.Fatalf("failure should retain pending intent with cooldown, got %+v", state)
	}
	if _, owned := cache.refresh(context.Background(), "a"); owned || calls != 1 {
		t.Fatal("retry ran before cooldown")
	}
	now = state.NextAllowedAt.Add(time.Millisecond)
	result, owned := cache.refresh(context.Background(), "a")
	if !owned || !result.Success || calls != 2 {
		t.Fatalf("retry not recovered: %+v owned=%v calls=%d", result, owned, calls)
	}
	state = cache.read("a")
	if !state.HasSnapshot || state.Pending || !state.NextAllowedAt.IsZero() {
		t.Fatalf("success not published/cleared: %+v", state)
	}
}

func TestPhase3CountsAllFourteenOfficialFieldsFromCompleteSources(t *testing.T) {
	app, _ := countsTestApp(t)
	names := []string{"AlbumCount", "ArtistCount", "BookCount", "BoxSetCount", "GameCount", "GameSystemCount", "ItemCount", "MusicVideoCount", "ProgramCount", "SongCount", "TrailerCount"}
	for i, id := range []string{"a", "b"} {
		state := app.mediaCounts.cache.states[id]
		state.snapshot.Extra = make(map[string]int64)
		for _, name := range names {
			state.snapshot.Extra[name] = int64(i + 1)
		}
	}
	rr := countsTestRequest(app.Handler(), http.MethodGet, "/emby/Items/Counts", "fixture-local-token")
	if rr.Code != 200 {
		t.Fatalf("complete response status %d: %s", rr.Code, rr.Body.String())
	}
	var body map[string]int64
	if err := json.Unmarshal(rr.Body.Bytes(), &body); err != nil {
		t.Fatal(err)
	}
	if len(body) != 14 {
		t.Fatalf("expected 14 official fields, got %d: %v", len(body), body)
	}
	for _, name := range names {
		if body[name] != 3 {
			t.Fatalf("%s aggregated %d expected 3", name, body[name])
		}
	}
	if body["MovieCount"] != 30 || body["SeriesCount"] != 3 || body["EpisodeCount"] != 300 {
		t.Fatalf("original 3 changed: %v", body)
	}
}

func TestPhase3CountsOptionalFieldAbsentFromOneSourceIsNotFabricated(t *testing.T) {
	app, _ := countsTestApp(t)
	app.mediaCounts.cache.states["a"].snapshot.Extra = map[string]int64{"SongCount": 21}
	rr := countsTestRequest(app.Handler(), http.MethodGet, "/Items/Counts", "fixture-local-token")
	if rr.Code != 200 {
		t.Fatalf("core fields should still be reliable; %d", rr.Code)
	}
	var payload map[string]any
	if err := json.Unmarshal(rr.Body.Bytes(), &payload); err != nil {
		t.Fatal(err)
	}
	if _, exists := payload["SongCount"]; exists {
		t.Fatalf("cannot report incomplete source SongCount as a complete aggregate: %v", payload)
	}
}

func TestPhase3CountsDecodeOfficialExtrasStrictly(t *testing.T) {
	full := `{"MovieCount":3,"SeriesCount":4,"EpisodeCount":5,"AlbumCount":6,"ArtistCount":7,"BookCount":8,"BoxSetCount":9,"GameCount":10,"GameSystemCount":11,"ItemCount":12,"MusicVideoCount":13,"ProgramCount":14,"SongCount":15,"TrailerCount":16}`
	base, extras, err := decodeMediaCountsWithExtras([]byte(full))
	if err != nil || base != (mediaCounts{3, 4, 5}) || len(extras) != 11 || extras["TrailerCount"] != 16 {
		t.Fatalf("real Emby-shaped payload not preserved: base=%+v extras=%v err=%v", base, extras, err)
	}
	cases := []string{
		`{"MovieCount":1,"SeriesCount":2,"EpisodeCount":3,"SongCount":"3"}`,
		`{"MovieCount":1,"SeriesCount":2,"EpisodeCount":3,"SongCount":-1}`,
		`{"MovieCount":1,"SeriesCount":2,"EpisodeCount":3,"SongCount":2147483648}`,
		`{"MovieCount":1,"SeriesCount":2,"EpisodeCount":3,"SongCount":null}`,
	}
	for _, body := range cases {
		value, fields, err := decodeMediaCountsWithExtras([]byte(body))
		if err != nil || value != (mediaCounts{1, 2, 3}) || len(fields) != 0 {
			t.Fatalf("optional malformed count must not invalidate core: value=%+v fields=%v err=%v body=%s", value, fields, err, body)
		}
	}
	if _, _, err := decodeMediaCountsWithExtras([]byte(`{"MovieCount":1,"SeriesCount":2,"EpisodeCount":3,"SongCount":3,"SongCount":4}`)); err == nil {
		t.Fatal("duplicate official key silently accepted")
	}
	three, absent, err := decodeMediaCountsWithExtras([]byte(`{"MovieCount":1,"SeriesCount":2,"EpisodeCount":3}`))
	if err != nil || three != (mediaCounts{1, 2, 3}) || len(absent) != 0 {
		t.Fatalf("three-count-compatible server rejected: %+v %v %v", three, absent, err)
	}
}

func TestPhase3CountsSchedulerWakesAtRetryDeadline(t *testing.T) {
	clock := newCountsTestClock()
	entered := make(chan int, 3)
	var calls atomic.Int32
	app := countsTestScheduleApp(t, clock, countsTestCollectorFunc(func(context.Context, countsSourceSpec) countsRoundResult {
		n := int(calls.Add(1))
		entered <- n
		if n == 1 {
			return countsRoundResult{Class: countsTransport, CountsCalls: 1}
		}
		return countsRoundResult{Success: true, Value: mediaCounts{1, 2, 3}}
	}), 1)
	app.mediaCounts.start()
	if got := countsTestReceive(t, entered); got != 1 {
		t.Fatalf("expected first failure, got %d", got)
	}
	countsTestEventually(t, func() bool {
		view := app.mediaCounts.cache.read("a")
		return !view.InFlight && view.Pending && view.NextAllowedAt.After(clock.now())
	})
	// Wait until the scheduler has actually installed its wake-up timer.
	countsTestEventually(t, func() bool {
		clock.mu.Lock()
		defer clock.mu.Unlock()
		for _, edge := range clock.edges {
			if edge.interval == 0 && !edge.stopped {
				return true
			}
		}
		return false
	})
	clock.advance(5 * time.Minute)
	if got := countsTestReceive(t, entered); got != 2 {
		t.Fatalf("background retry was not dispatched, got %d", got)
	}
	countsTestEventually(t, func() bool { return app.mediaCounts.cache.read("a").HasSnapshot })
	if calls.Load() != 2 {
		t.Fatalf("unexpected extra work: %d", calls.Load())
	}
}

func TestPhase3CountsKnownRateLimitDoesNotUseGenericRetry(t *testing.T) {
	now := time.Unix(1700000000, 0)
	cache := countsTestCache(t, func() time.Time { return now }, countsTestCollectorFunc(func(context.Context, countsSourceSpec) countsRoundResult {
		return countsRoundResult{Class: countsLimited, Wait: countsWait{Limited: true, HasServerWait: true, Until: now.Add(45 * time.Minute)}}
	}))
	cache.syncSource(cache.prepareSource(countsTestClient("a"), nil, ""))
	cache.refresh(context.Background(), "a")
	state := cache.read("a")
	if !state.Pending || !state.NextAllowedAt.Equal(now.Add(45*time.Minute)) {
		t.Fatalf("server retry-after must have precedence: %+v", state)
	}
}

func TestPhase3CountsExistingSnapshotSurvivesTransientError(t *testing.T) {
	now := time.Unix(1700000000, 0)
	calls := 0
	cache := countsTestCache(t, func() time.Time { return now }, countsTestCollectorFunc(func(context.Context, countsSourceSpec) countsRoundResult {
		calls++
		if calls == 1 {
			return countsRoundResult{Success: true, Value: mediaCounts{11, 12, 13}, Extra: map[string]int64{"SongCount": 14}}
		}
		return countsRoundResult{Class: countsTransport}
	}))
	cache.syncSource(cache.prepareSource(countsTestClient("a"), nil, ""))
	cache.refresh(context.Background(), "a")
	now = now.Add(time.Minute)
	cache.refresh(context.Background(), "a")
	view := cache.read("a")
	if !view.HasSnapshot || view.Snapshot.Value.MovieCount != 11 || view.Snapshot.Extra["SongCount"] != 14 || view.Pending {
		t.Fatalf("prior genuine snapshot lost or unnecessary retry queued: %+v", view)
	}
}

func TestPhase3CountsRetryBackoffCapsAtFortyMinutes(t *testing.T) {
	now := time.Unix(1700000000, 0)
	cache := countsTestCache(t, func() time.Time { return now }, countsTestCollectorFunc(func(context.Context, countsSourceSpec) countsRoundResult {
		return countsRoundResult{Class: countsTransport, CountsCalls: 1}
	}))
	cache.syncSource(cache.prepareSource(countsTestClient("a"), nil, ""))
	for _, wait := range []time.Duration{5 * time.Minute, 10 * time.Minute, 20 * time.Minute, 40 * time.Minute, 40 * time.Minute} {
		result, owned := cache.refresh(context.Background(), "a")
		if !owned || result.Success {
			t.Fatalf("expected one failed owned collection: %+v owned=%v", result, owned)
		}
		view := cache.read("a")
		if !view.Pending || !view.NextAllowedAt.Equal(now.Add(wait)) {
			t.Fatalf("retry interval=%v expected=%v", view.NextAllowedAt.Sub(now), wait)
		}
		now = view.NextAllowedAt
	}
}

func TestPhase3CountsOfficialExtrasRespectUserScopeAndHEAD(t *testing.T) {
	app, _ := countsTestApp(t)
	app.mediaCounts.cache.states["a"].snapshot.Extra = map[string]int64{"SongCount": 10}
	app.mediaCounts.cache.states["b"].snapshot.Extra = map[string]int64{"SongCount": 20}
	handler := app.Handler()
	admin := countsTestRequest(handler, http.MethodGet, "/Items/Counts", "fixture-admin-token")
	var adminData map[string]int64
	if err := json.Unmarshal(admin.Body.Bytes(), &adminData); err != nil || adminData["SongCount"] != 30 {
		t.Fatalf("admin optional sum incorrect: %v err=%v", adminData, err)
	}
	app.UserStore.mu.Lock()
	app.UserStore.users["alice"].AllowedServers = []string{"a"}
	app.UserStore.mu.Unlock()
	user := countsTestRequest(handler, http.MethodGet, "/Items/Counts", "fixture-local-token")
	var userData map[string]int64
	if err := json.Unmarshal(user.Body.Bytes(), &userData); err != nil || userData["SongCount"] != 10 {
		t.Fatalf("user scope leaked another upstream: %v err=%v", userData, err)
	}
	head := countsTestRequest(handler, http.MethodHead, "/Items/Counts", "fixture-local-token")
	if head.Code != user.Code || head.Body.Len() != 0 || head.Header().Get("Content-Length") != strconv.Itoa(user.Body.Len()) {
		t.Fatalf("HEAD differs from official extended GET representation: code=%d len=%s", head.Code, head.Header().Get("Content-Length"))
	}
}

func TestPhase3CountsNewSourceGenerationDoesNotInheritTransientRetry(t *testing.T) {
	now := time.Unix(1700000000, 0)
	cache := countsTestCache(t, func() time.Time { return now }, countsTestCollectorFunc(func(context.Context, countsSourceSpec) countsRoundResult {
		return countsRoundResult{Class: countsTransport}
	}))
	client := countsTestClient("a")
	cache.syncSource(cache.prepareSource(client, nil, ""))
	cache.refresh(context.Background(), "a")
	before := cache.read("a")
	if !before.Pending || !before.NextAllowedAt.After(now) {
		t.Fatalf("initial failure had no retry schedule: %+v", before)
	}
	// Authentication remains current; only the configured outbound identity
	// changes. Old transient source failure must not delay the new generation.
	client.Config.CustomUserAgent = "changed-client-identity"
	cache.syncSource(cache.prepareSource(client, nil, ""))
	after := cache.read("a")
	if !after.NextAllowedAt.IsZero() || !after.Pending {
		t.Fatalf("changed source inherited old transient backoff: %+v", after)
	}
}

func TestPhase3CountsSourceGenerationPreservesServerRetryAfter(t *testing.T) {
	now := time.Unix(1700000000, 0)
	cache := countsTestCache(t, func() time.Time { return now }, countsTestCollectorFunc(func(context.Context, countsSourceSpec) countsRoundResult {
		return countsRoundResult{Class: countsLimited, Wait: countsWait{Limited: true, HasServerWait: true, Until: now.Add(45 * time.Minute)}}
	}))
	client := countsTestClient("a")
	cache.syncSource(cache.prepareSource(client, nil, ""))
	cache.refresh(context.Background(), "a")
	client.Config.CustomUserAgent = "changed-client-identity"
	cache.syncSource(cache.prepareSource(client, nil, ""))
	if got := cache.read("a").NextAllowedAt; !got.Equal(now.Add(45 * time.Minute)) {
		t.Fatalf("source change bypassed the upstream's rate limit: %v", got)
	}
}
