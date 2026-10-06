package backend

import (
	"context"
	"sync/atomic"
	"testing"
	"time"
)

func TestMediaCountsCacheAtomicReplacementAndFailure(t *testing.T) {
	now := time.Unix(1700000000, 0)
	outcomes := []countsRoundResult{
		{Success: true, Value: mediaCounts{5, 6, 7}},
		{Class: countsHTTP},
		{Success: true, Value: mediaCounts{8, -1, 9}},
		{Success: true, Value: mediaCounts{}},
	}
	calls := 0
	cache := countsTestCache(t, func() time.Time { return now }, countsTestCollectorFunc(func(context.Context, countsSourceSpec) countsRoundResult {
		got := outcomes[calls]
		calls++
		return got
	}))
	client := countsTestClient("a")
	cache.syncSource(cache.prepareSource(client, nil, ""))
	cache.refresh(context.Background(), "a")
	original := cache.read("a").Snapshot
	if original.Value != (mediaCounts{5, 6, 7}) {
		t.Fatal("missing complete success")
	}
	for i := 0; i < 2; i++ {
		now = now.Add(time.Hour)
		cache.refresh(context.Background(), "a")
		if got := cache.read("a"); !got.HasSnapshot || got.Snapshot != original {
			t.Fatal("failure/invalid triple damaged complete success")
		}
	}
	cache.refresh(context.Background(), "a")
	if got := cache.read("a"); !got.HasSnapshot || got.Snapshot.Value != (mediaCounts{}) {
		t.Fatal("successful zero did not replace")
	}
}

func TestMediaCountsCacheGenerationAndCooldown(t *testing.T) {
	now := time.Unix(1700000000, 0)
	cache := countsTestCache(t, func() time.Time { return now }, countsTestCollectorFunc(func(context.Context, countsSourceSpec) countsRoundResult {
		return countsRoundResult{Success: true, Value: mediaCounts{7, 8, 9}}
	}))
	client := countsTestClient("a")
	cache.syncSource(cache.prepareSource(client, nil, ""))
	cache.refresh(context.Background(), "a")
	original := cache.read("a")
	client.mu.Lock()
	client.AccessToken = "new-fixture-token"
	client.apiObservation++
	client.mu.Unlock()
	cache.syncSource(cache.prepareSource(client, nil, ""))
	if got := cache.read("a"); !got.HasSnapshot || got.Snapshot != original.Snapshot || got.WorkGeneration == original.WorkGeneration {
		t.Fatal("same account token rotation lost old success or retained work lease")
	}
	cache.mu.Lock()
	state := cache.states["a"]
	cache.applyWaitLocked(state, countsWait{Limited: true})
	cooldown := state.nextAllowedAt
	cache.mu.Unlock()
	replacement := countsTestClient("a")
	replacement.Config.Name = "renamed"
	replacement.Name = "renamed"
	cache.syncSource(cache.prepareSource(replacement, nil, ""))
	if got := cache.read("a"); !got.HasSnapshot || !got.NextAllowedAt.Equal(cooldown) {
		t.Fatal("same-data reload lost cache/wait")
	}
	replacement = countsTestClient("a")
	replacement.Config.Password = "different-fixture-account"
	replacement.countsLoginConfig = replacement.configuredCountsLoginBinding()
	cache.syncSource(cache.prepareSource(replacement, nil, ""))
	if got := cache.read("a"); got.HasSnapshot || !got.NextAllowedAt.Equal(cooldown) {
		t.Fatal("account change leaked old cache or bypassed wait")
	}
	cache.requestRefresh("a")
	if _, owned := cache.refresh(context.Background(), "a"); owned {
		t.Fatal("refresh bypassed cooldown")
	}
	cache.removeSource("a")
	cache.syncSource(cache.prepareSource(countsTestClient("a"), nil, ""))
	if got := cache.read("a"); got.HasSnapshot || !got.NextAllowedAt.IsZero() {
		t.Fatal("deleted source reincarnation inherited state")
	}
}

func TestMediaCountsCacheInFlightJoinAndLateDelete(t *testing.T) {
	for _, remove := range []bool{false, true} {
		t.Run(map[bool]string{false: "join", true: "deleted-late-success"}[remove], func(t *testing.T) {
			started := make(chan struct{})
			release := make(chan struct{})
			var calls atomic.Int32
			cache := countsTestCache(t, nil, countsTestCollectorFunc(func(ctx context.Context, _ countsSourceSpec) countsRoundResult {
				calls.Add(1)
				close(started)
				select {
				case <-release:
				case <-ctx.Done():
				}
				return countsRoundResult{Success: true, Value: mediaCounts{99, 99, 99}}
			}))
			cache.syncSource(cache.prepareSource(countsTestClient("a"), nil, ""))
			owner := make(chan countsRoundResult, 1)
			go func() { r, _ := cache.refresh(context.Background(), "a"); owner <- r }()
			countsTestReceive(t, started)
			if remove {
				cache.removeSource("a")
				cache.syncSource(cache.prepareSource(countsTestClient("a"), nil, ""))
				close(release)
				countsTestReceive(t, owner)
				if cache.read("a").HasSnapshot {
					t.Fatal("late deleted lease revived counts")
				}
			} else {
				for i := 0; i < 20; i++ {
					cache.requestRefresh("a")
				}
				if cache.read("a").Pending {
					t.Fatal("valid flight triggers queued an extra round")
				}
				joinCtx, cancel := context.WithCancel(context.Background())
				joined := make(chan bool, 1)
				go func() { _, owned := cache.refresh(joinCtx, "a"); joined <- owned }()
				cancel()
				if countsTestReceive(t, joined) {
					t.Fatal("joiner owned flight")
				}
				close(release)
				countsTestReceive(t, owner)
				if !cache.read("a").HasSnapshot || calls.Load() != 1 {
					t.Fatal("single-flight failure")
				}
			}
		})
	}
}

func TestMediaCountsCacheWaitEscalationAndNoShortening(t *testing.T) {
	now := time.Unix(1700000000, 0)
	cache := countsTestCache(t, func() time.Time { return now }, countsTestCollectorFunc(func(context.Context, countsSourceSpec) countsRoundResult { return countsRoundResult{} }))
	state := &countsSourceState{}
	for _, minutes := range []int{5, 10, 20, 40, 60, 60} {
		cache.applyWaitLocked(state, countsWait{Limited: true, NeedsFallback: true})
		if !state.nextAllowedAt.Equal(now.Add(time.Duration(minutes) * time.Minute)) {
			t.Fatal("wrong fallback wait")
		}
		now = state.nextAllowedAt
	}
	far := now.Add(6 * time.Hour)
	cache.applyWaitLocked(state, countsWait{Limited: true, HasServerWait: true, Until: far})
	cache.applyWaitLocked(state, countsWait{Limited: true, HasServerWait: true, Until: now.Add(time.Minute)})
	if !state.nextAllowedAt.Equal(far) || state.limitedFailures != 0 || !state.pending {
		t.Fatal("server wait shortened")
	}
}

func TestMediaCountsCacheCloseCancelsAndRejectsWork(t *testing.T) {
	started := make(chan struct{})
	canceled := make(chan struct{})
	cache := countsTestCache(t, nil, countsTestCollectorFunc(func(ctx context.Context, _ countsSourceSpec) countsRoundResult {
		close(started)
		<-ctx.Done()
		close(canceled)
		return countsRoundResult{Class: countsCanceled}
	}))
	cache.syncSource(cache.prepareSource(countsTestClient("a"), nil, ""))
	finished := make(chan struct{})
	go func() { cache.refresh(context.Background(), "a"); close(finished) }()
	countsTestReceive(t, started)
	closed := make(chan struct{})
	go func() { cache.close(); close(closed) }()
	countsTestReceive(t, canceled)
	countsTestReceive(t, finished)
	countsTestReceive(t, closed)
	if result, owned := cache.refresh(context.Background(), "a"); owned || result.Class != countsClosed {
		t.Fatal("work after close")
	}
	if cache.requestRefresh("a") || cache.read("a").HasSnapshot {
		t.Fatal("state survived close")
	}
}

func TestMediaCountsCacheOfflineObservationDoesNotBeatNewLogin(t *testing.T) {
	for _, newLogin := range []bool{false, true} {
		t.Run(map[bool]string{false: "current-offline", true: "new-login-wins"}[newLogin], func(t *testing.T) {
			client := countsTestClient("a")
			calls := 0
			cache := countsTestCache(t, nil, countsTestCollectorFunc(func(context.Context, countsSourceSpec) countsRoundResult {
				calls++
				if calls == 1 {
					return countsRoundResult{Success: true, Value: mediaCounts{10, 1, 100}}
				}
				if newLogin {
					client.mu.Lock()
					client.apiObservation++
					client.AccessToken = "new-login-token"
					client.mu.Unlock()
				}
				return countsRoundResult{Class: countsTransport, Reachability: countsReachabilityOffline, CountsCalls: 1, CheckCalls: 1}
			}))
			cache.syncSource(cache.prepareSource(client, nil, ""))
			cache.refresh(context.Background(), "a")
			old := cache.read("a").Snapshot
			cache.refresh(context.Background(), "a")
			if client.IsOnline() != newLogin {
				t.Fatal("offline result superseded a newer login or failed to publish")
			}
			if got := cache.read("a"); !got.HasSnapshot || got.Snapshot != old {
				t.Fatal("offline observation destroyed old complete value")
			}
		})
	}
}
