package backend

import (
	"context"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

type countsTestClockEdge struct {
	at       time.Time
	interval time.Duration
	ch       chan time.Time
	stopped  bool
}
type countsTestClock struct {
	mu          sync.Mutex
	instant     time.Time
	edges       []*countsTestClockEdge
	tickerCalls int
}

func newCountsTestClock() *countsTestClock {
	return &countsTestClock{instant: time.Unix(1700000000, 0)}
}
func (clock *countsTestClock) now() time.Time {
	clock.mu.Lock()
	defer clock.mu.Unlock()
	return clock.instant
}
func (clock *countsTestClock) edge(delay, interval time.Duration) (<-chan time.Time, func()) {
	clock.mu.Lock()
	edge := &countsTestClockEdge{at: clock.instant.Add(delay), interval: interval, ch: make(chan time.Time, 8)}
	clock.edges = append(clock.edges, edge)
	if interval > 0 {
		clock.tickerCalls++
	}
	clock.mu.Unlock()
	return edge.ch, func() { clock.mu.Lock(); edge.stopped = true; clock.mu.Unlock() }
}
func (clock *countsTestClock) injected() countsScheduleClock {
	return countsScheduleClock{now: clock.now,
		ticker: func(d time.Duration) (<-chan time.Time, func()) { return clock.edge(d, d) },
		timer:  func(d time.Duration) (<-chan time.Time, func()) { return clock.edge(d, 0) },
	}
}
func (clock *countsTestClock) advance(delta time.Duration) {
	clock.mu.Lock()
	defer clock.mu.Unlock()
	clock.instant = clock.instant.Add(delta)
	for _, edge := range clock.edges {
		for !edge.stopped && !edge.at.After(clock.instant) {
			select {
			case edge.ch <- edge.at:
			default:
			}
			if edge.interval == 0 {
				edge.stopped = true
			} else {
				edge.at = edge.at.Add(edge.interval)
			}
		}
	}
}
func (clock *countsTestClock) tickerCount() int {
	clock.mu.Lock()
	defer clock.mu.Unlock()
	return clock.tickerCalls
}

func countsTestScheduleApp(t *testing.T, clock *countsTestClock, collector countsCollector, n int) *App {
	t.Helper()
	cfg := &Config{}
	pool := &UpstreamPool{}
	for i := 0; i < n; i++ {
		client := countsTestClient(string(rune('a' + i)))
		cfg.Upstream = append(cfg.Upstream, client.Config)
		pool.clients = append(pool.clients, client)
	}
	app := &App{ConfigStore: &ConfigStore{config: cfg}, Upstream: pool}
	service, err := newMediaCountsService(app, clock.injected(), collector)
	if err != nil {
		t.Fatal(err)
	}
	app.mediaCounts = service
	t.Cleanup(service.close)
	return app
}

func TestMediaCountsSchedulerBoundedWorkersAndClose(t *testing.T) {
	clock := newCountsTestClock()
	entered := make(chan string, 8)
	permits := make(chan struct{})
	var active, peak, canceled atomic.Int32
	collector := countsTestCollectorFunc(func(ctx context.Context, source countsSourceSpec) countsRoundResult {
		current := active.Add(1)
		defer active.Add(-1)
		for old := peak.Load(); current > old; old = peak.Load() {
			if peak.CompareAndSwap(old, current) {
				break
			}
		}
		deadline, ok := ctx.Deadline()
		if !ok || time.Until(deadline) > countsRoundTimeout {
			return countsRoundResult{Class: countsNotReady}
		}
		entered <- source.Client.ID
		select {
		case <-permits:
			return countsRoundResult{Success: true, Value: mediaCounts{1, 1, 1}}
		case <-ctx.Done():
			canceled.Add(1)
			return countsRoundResult{Class: countsCanceled}
		}
	})
	app := countsTestScheduleApp(t, clock, collector, 3)
	app.mediaCounts.start()
	first, second := countsTestReceive(t, entered), countsTestReceive(t, entered)
	if first == second || active.Load() != 2 {
		t.Fatal("expected two different active sources")
	}
	select {
	case <-entered:
		t.Fatal("third source started before a slot was free")
	default:
	}
	permits <- struct{}{}
	third := countsTestReceive(t, entered)
	if third == first || third == second || peak.Load() > 2 {
		t.Fatal("worker limit/source dispatch")
	}
	closed := make(chan struct{})
	go func() { app.mediaCounts.close(); close(closed) }()
	countsTestReceive(t, closed)
	if active.Load() != 0 || canceled.Load() != 2 || !app.mediaCounts.closed.Load() {
		t.Fatal("close did not cancel/wait workers")
	}
}

func TestMediaCountsSchedulerFixedHourAndExtraBinding(t *testing.T) {
	clock := newCountsTestClock()
	entered := make(chan time.Time, 8)
	collector := countsTestCollectorFunc(func(context.Context, countsSourceSpec) countsRoundResult {
		entered <- clock.now()
		return countsRoundResult{Success: true, Value: mediaCounts{1, 2, 3}}
	})
	app := countsTestScheduleApp(t, clock, collector, 1)
	app.mediaCounts.start()
	start := countsTestReceive(t, entered)
	countsTestEventually(t, func() bool {
		return app.mediaCounts.cache.read("a").HasSnapshot && !app.mediaCounts.cache.read("a").InFlight
	})
	clock.advance(10 * time.Minute)
	app.watchLifecycleMu.Lock()
	app.requestBoundCountsLocked([]string{"a", "a"})
	app.watchLifecycleMu.Unlock()
	extra := countsTestReceive(t, entered)
	if !extra.Equal(start.Add(10 * time.Minute)) {
		t.Fatal("extra binding not immediate")
	}
	countsTestEventually(t, func() bool { return !app.mediaCounts.cache.read("a").InFlight })
	clock.advance(50 * time.Minute)
	hourly := countsTestReceive(t, entered)
	if !hourly.Equal(start.Add(time.Hour)) || clock.tickerCount() != 1 {
		t.Fatal("extra binding moved/replaced hourly ticker")
	}
}

func TestMediaCountsSchedulerCooldownAndQueuedLease(t *testing.T) {
	clock := newCountsTestClock()
	var calls atomic.Int32
	app := countsTestScheduleApp(t, clock, countsTestCollectorFunc(func(context.Context, countsSourceSpec) countsRoundResult { calls.Add(1); return countsRoundResult{} }), 1)
	cache := app.mediaCounts.cache
	app.watchLifecycleMu.Lock()
	app.syncCountsSourcesLocked(app.ConfigStore.Snapshot(), true)
	app.watchLifecycleMu.Unlock()
	cache.mu.Lock()
	state := cache.states["a"]
	state.nextAllowedAt = clock.now().Add(2 * time.Hour)
	oldJob := countsScheduledJob{"a", state.incarnation, state.workGeneration}
	cache.mu.Unlock()
	for i := 0; i < 10; i++ {
		app.watchLifecycleMu.Lock()
		app.requestBoundCountsLocked([]string{"a"})
		app.watchLifecycleMu.Unlock()
	}
	jobs, next := app.countsSchedule(clock.now(), map[string]bool{}, true)
	if len(jobs) != 0 || !next.Equal(clock.now().Add(2*time.Hour)) || calls.Load() != 0 {
		t.Fatal("trigger bypassed cooldown")
	}
	clock.advance(2 * time.Hour)
	jobs, _ = app.countsSchedule(clock.now(), map[string]bool{}, false)
	if len(jobs) != 1 {
		t.Fatal("cooldown expiry did not dispatch merged intent")
	}
	app.Upstream.clients[0].mu.Lock()
	app.Upstream.clients[0].AccessToken = "new-token"
	app.Upstream.clients[0].apiObservation++
	app.Upstream.clients[0].mu.Unlock()
	app.watchLifecycleMu.Lock()
	app.syncCountsSourcesLocked(app.ConfigStore.Snapshot(), false)
	app.watchLifecycleMu.Unlock()
	if app.countsJobCurrent(context.Background(), oldJob) {
		t.Fatal("old queued work lease accepted")
	}
	app.lifecyclePending = true
	jobs, _ = app.countsSchedule(clock.now(), map[string]bool{}, true)
	if len(jobs) != 0 {
		t.Fatal("pending lifecycle dispatched")
	}
}

func TestMediaCountsBindingSetComparison(t *testing.T) {
	if !countsSameBindings([]string{"b", "a", "a"}, []string{"a", "b"}) || !countsSameBindings(nil, []string{}) ||
		countsSameBindings([]string{"a"}, []string{"b"}) {
		t.Fatal("binding changes confused with reorder/duplicates")
	}
}

func TestMediaCountsLifecycleGateCancelsBeforePublication(t *testing.T) {
	app := &App{}
	app.watchLifecycleMu.Lock()
	ctx, cancel := context.WithCancel(context.Background())
	finished := make(chan bool, 1)
	go func() {
		release, ok := app.lockCountsLifecycle(ctx)
		if release != nil {
			release()
		}
		finished <- ok
	}()
	cancel()
	locked := countsTestReceive(t, finished)
	app.watchLifecycleMu.Unlock()
	if locked {
		t.Fatal("canceled waiter acquired lifecycle gate")
	}
}
