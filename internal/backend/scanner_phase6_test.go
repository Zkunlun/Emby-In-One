package backend

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"sort"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"
)

type phase6MockSource struct {
	label                string
	server               *httptest.Server
	mu                   sync.Mutex
	calls                []task6HTTPCall
	rows                 []map[string]any
	libraries            []map[string]any
	viewsTotalOverride   int
	repeatPages          bool
	deltaFilterSupported bool
	failStatus           int
	pageStarted          chan struct{}
	pageContinue         chan struct{}
	pageOnce             sync.Once
	inFlight             int
	maxInFlight          int
}

func newPhase6MockSource(t *testing.T, label string, rows []map[string]any) *phase6MockSource {
	t.Helper()
	x := &phase6MockSource{label: label, rows: rows,
		libraries: []map[string]any{{"Id": "library-" + label, "CollectionType": "movies"},
			{"Id": "audio-" + label, "CollectionType": "music"}}}
	x.server = httptest.NewServer(http.HandlerFunc(x.serve))
	t.Cleanup(x.server.Close)
	return x
}
func (x *phase6MockSource) serve(w http.ResponseWriter, r *http.Request) {
	x.mu.Lock()
	x.calls = append(x.calls, task6HTTPCall{Path: r.URL.Path, Query: r.URL.Query()})
	fail, gate, release := x.failStatus, x.pageStarted, x.pageContinue
	x.mu.Unlock()
	write := func(v any) { w.Header().Set("Content-Type", "application/json"); _ = json.NewEncoder(w).Encode(v) }
	switch r.URL.Path {
	case "/Users/AuthenticateByName":
		write(map[string]any{"AccessToken": "up-" + x.label, "User": map[string]any{"Id": "user-" + x.label}})
		return
	case "/System/Info/Public":
		write(map[string]any{"Id": "source-" + x.label})
		return
	case "/Items/Counts":
		write(map[string]any{"MovieCount": len(x.rows), "SeriesCount": 0, "EpisodeCount": 0})
		return
	case "/Users/user-" + x.label + "/Views":
		total := len(x.libraries)
		if x.viewsTotalOverride > 0 {
			total = x.viewsTotalOverride
		}
		write(map[string]any{"Items": x.libraries, "TotalRecordCount": total, "StartIndex": 0})
		return
	case "/Users/user-" + x.label + "/Items":
		x.mu.Lock()
		x.inFlight++
		if x.inFlight > x.maxInFlight {
			x.maxInFlight = x.inFlight
		}
		x.mu.Unlock()
		defer func() { x.mu.Lock(); x.inFlight--; x.mu.Unlock() }()
		if gate != nil {
			x.pageOnce.Do(func() { close(gate) })
			select {
			case <-release:
			case <-r.Context().Done():
				return
			}
		}
		if fail != 0 {
			w.WriteHeader(fail)
			_, _ = w.Write([]byte("denied"))
			return
		}
		q := r.URL.Query()
		start, _ := strconv.Atoi(q.Get("StartIndex"))
		limit, _ := strconv.Atoi(q.Get("Limit"))
		if start < 0 || limit <= 0 || limit > scannerPageSize {
			http.Error(w, "bad scan window", 400)
			return
		}
		parent := q.Get("ParentId")
		valid := false
		for _, lib := range x.libraries {
			if lib["Id"] == parent && lib["CollectionType"] != "music" {
				valid = true
			}
		}
		if !valid {
			http.NotFound(w, r)
			return
		}
		rows := []map[string]any{}
		for _, item := range x.rows {
			if kind, _ := item["Type"].(string); kind != "Movie" && kind != "Series" {
				continue
			}
			itemParent, _ := item["ParentId"].(string)
			if itemParent == "" {
				itemParent = "library-" + x.label
			}
			if itemParent != parent {
				continue
			}
			rows = append(rows, item)
		}
		if min := q.Get("MinDateLastSaved"); min != "" && x.deltaFilterSupported {
			boundary, ok := parseScannerTime(min)
			if !ok {
				http.Error(w, "invalid delta cutoff", 400)
				return
			}
			filtered := make([]map[string]any, 0, len(rows))
			for _, item := range rows {
				saved, ok := parseScannerTime(item["DateLastSaved"])
				if ok && !saved.Before(boundary) {
					filtered = append(filtered, item)
				}
			}
			rows = filtered
		}
		key := q.Get("SortBy")
		desc := strings.EqualFold(q.Get("SortOrder"), "Descending")
		sort.SliceStable(rows, func(i, j int) bool {
			field := "Name"
			if key == "DateCreated" || key == "DateLastSaved" {
				field = key
			}
			left, right := fmt.Sprint(rows[i][field]), fmt.Sprint(rows[j][field])
			if left == right {
				return false
			}
			if desc {
				return left > right
			}
			return left < right
		})
		count := len(rows)
		requestedStart := start
		if x.repeatPages && start > 0 {
			start = 0
			count = 10000
		}
		if start > len(rows) {
			start = len(rows)
		}
		end := start + limit
		if end > len(rows) {
			end = len(rows)
		}
		write(map[string]any{"Items": rows[start:end], "StartIndex": requestedStart, "TotalRecordCount": count})
		return
	default:
		http.NotFound(w, r)
	}
}
func (x *phase6MockSource) onlyScannerCalls() []task6HTTPCall {
	x.mu.Lock()
	defer x.mu.Unlock()
	out := []task6HTTPCall{}
	for _, call := range x.calls {
		if strings.HasSuffix(call.Path, "/Items") || strings.HasSuffix(call.Path, "/Views") {
			out = append(out, call)
		}
	}
	return out
}
func phase6SampleMovies(prefix string, count int, providerStart int) []map[string]any {
	out := make([]map[string]any, 0, count)
	for i := 0; i < count; i++ {
		out = append(out, map[string]any{"Id": fmt.Sprintf("%s-%03d", prefix, i), "Type": "Movie",
			"Name": fmt.Sprintf("Movie %05d", i), "ProductionYear": 2024,
			"ProviderIds": map[string]any{"Tmdb": fmt.Sprintf("%d", providerStart+i)}})
	}
	return out
}
func withPhase6MockApp(t *testing.T, a, b *phase6MockSource, fn func(*App)) {
	t.Helper()
	config := phase1EDualPlaybackConfig(a.server.URL, b.server.URL)
	withTempAppConfig(t, config, func(app *App, _ http.Handler) {
		app.Scanner.wait = func(ctx context.Context, d time.Duration) error {
			if d < 5*time.Second || d > 20*time.Second {
				t.Errorf("scanner pacing out of expected range: %v", d)
			}
			return ctx.Err()
		}
		fn(app)
	})
}
func TestPhase6A6BInitialFullBoundedAndNoUnauthorizedFanout(t *testing.T) {
	a := newPhase6MockSource(t, "a", phase6SampleMovies("a", 125, 10000))
	b := newPhase6MockSource(t, "b", phase6SampleMovies("b", 125, 10000))
	withPhase6MockApp(t, a, b, func(app *App) {
		run := phase5Enable(t, app, "server-a")
		err := app.scannerRunFull(context.Background(), "server-a", run.ID)
		if err != nil {
			t.Fatal(err)
		}
		settings, err := app.Scanner.sourceLocked("server-a")
		if err != nil || !settings.InitialFullCompleted {
			t.Fatalf("initial full not marked: %+v %v", settings, err)
		}
		calls := a.onlyScannerCalls()
		views, pages := 0, 0
		for _, call := range calls {
			if strings.HasSuffix(call.Path, "/Views") {
				views++
				continue
			}
			pages++
			expected := strconv.Itoa((pages - 1) * 60)
			if call.Query.Get("StartIndex") != expected || call.Query.Get("Limit") != "60" ||
				call.Query.Get("Recursive") != "true" || call.Query.Get("IncludeItemTypes") != "Movie,Series" ||
				strings.Contains(call.Query.Get("Fields"), "MediaSources") || call.Query.Get("Ids") != "" {
				t.Fatalf("scanner query %d not bounded/qualified: %v", pages, call.Query)
			}
		}
		if views != 1 || pages != 3 {
			t.Fatalf("views=%d pages=%d, want 1 and 3", views, pages)
		}
		for _, call := range b.onlyScannerCalls() {
			t.Fatalf("unallowlisted B was scanned: %s", call.Path)
		}
		last, err := app.Scanner.runLocked("server-a", false)
		if err != nil || last.State != scanCompleted || last.Items != 125 || last.Pages != 3 {
			t.Fatalf("scan not completed correctly %+v %v", last, err)
		}
		id := app.IDStore.ResolveMergeMember("server-a", "a-124", "")
		if id == "" {
			t.Fatal("last page lacked durable WorkIdentity / MergeStore mapping")
		}
		status, err := app.scannerState("server-a")
		if err != nil {
			t.Fatal(err)
		}
		libs := status["libraries"].([]map[string]any)
		if len(libs) != 1 || libs[0]["fullSafeWatermark"] != run.StartedAt {
			t.Fatalf("incorrect full-start watermark: %+v", libs)
		}
	})
}
func TestPhase6MergeDiscoveryAcrossAuthorizedRuns(t *testing.T) {
	a := newPhase6MockSource(t, "a", phase6SampleMovies("a", 1, 99123))
	b := newPhase6MockSource(t, "b", phase6SampleMovies("b", 1, 99123))
	withPhase6MockApp(t, a, b, func(app *App) {
		run := phase5Enable(t, app, "server-a")
		if err := app.scannerRunFull(context.Background(), "server-a", run.ID); err != nil {
			t.Fatal(err)
		}
		if err := app.setScannerSource("server-b", true); err != nil {
			t.Fatal(err)
		}
		second, err := app.scannerCommand("server-b", "start")
		if err != nil {
			t.Fatal(err)
		}
		if err := app.scannerRunFull(context.Background(), "server-b", second.ID); err != nil {
			t.Fatal(err)
		}
		idA := app.IDStore.ResolveMergeMember("server-a", "a-000", "")
		idB := app.IDStore.ResolveMergeMember("server-b", "b-000", "")
		if idA == "" || idA != idB {
			t.Fatalf("scanner did not reuse shared strict merge service: %q %q", idA, idB)
		}
	})
}
func TestPhase6ActivityGateRequiresFullSixtySeconds(t *testing.T) {
	a := newPhase6MockSource(t, "a", phase6SampleMovies("a", 1, 555))
	b := newPhase6MockSource(t, "b", nil)
	withPhase6MockApp(t, a, b, func(app *App) {
		now := time.Date(2026, 10, 10, 1, 0, 0, 0, time.UTC)
		app.scanActivity.now = func() time.Time { return now }
		run := phase5Enable(t, app, "server-a")
		app.scanActivity.note([]string{"server-a"})
		if err := app.scannerRunFull(context.Background(), "server-a", run.ID); !errors.Is(err, errScannerHold) {
			t.Fatalf("activity allowed active scan: %v", err)
		}
		if n := len(a.onlyScannerCalls()); n != 0 {
			t.Fatalf("interactive activity failed to block Views: %d", n)
		}
		now = now.Add(59 * time.Second)
		if app.scanActivity.quiet("server-a") {
			t.Fatal("quiet window ended before 60s")
		}
		now = now.Add(2 * time.Second)
		if err := app.scannerRunFull(context.Background(), "server-a", run.ID); err != nil {
			t.Fatal(err)
		}
	})
}
