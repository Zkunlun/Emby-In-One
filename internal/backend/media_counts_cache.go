package backend

import (
	"context"
	"crypto/rand"
	"sync"
	"time"
)

type countsCollector interface {
	collect(context.Context, countsSourceSpec) countsRoundResult
}

type countsFlight struct {
	incarnation    uint64
	dataGeneration uint64
	workGeneration uint64
	roundID        uint64
	source         countsSourceSpec
	done           chan struct{}
	cancel         context.CancelFunc
	result         countsRoundResult // Published before done closes.
}

type countsSourceState struct {
	spec            countsSourceSpec
	incarnation     uint64
	dataGeneration  uint64
	workGeneration  uint64
	snapshot        countsSnapshot
	hasSnapshot     bool
	lastAttempt     time.Time
	lastErrorClass  countsErrorClass
	inFlight        *countsFlight
	pending         bool
	nextAllowedAt   time.Time
	limitedFailures uint8
}

type countsCacheView struct {
	Snapshot       countsSnapshot
	HasSnapshot    bool
	Online         bool
	Ready          bool
	InFlight       bool
	Pending        bool
	LastAttempt    time.Time
	LastErrorClass countsErrorClass
	NextAllowedAt  time.Time
	DataGeneration uint64
	WorkGeneration uint64
}

// The cache owns generations/leases and cancellation, not goroutines or timers.
// Phase2 supplies the bounded workers and successful configuration publication
// hooks. HTTP statistics handlers must only use read, never refresh.
type countsCache struct {
	mu        sync.RWMutex
	states    map[string]*countsSourceState
	key       [32]byte
	sequence  uint64
	closed    bool
	now       func() time.Time
	collector countsCollector
	// Set once before use by Phase2; acquired before the cache lock.
	publicationGate func(context.Context, string, countsSourceSpec) (func(), bool)
	running         sync.WaitGroup
}

func newCountsCache(now func() time.Time, collector countsCollector) (*countsCache, error) {
	if now == nil {
		now = time.Now
	}
	if collector == nil {
		collector = newCountsUpstreamCollector(now)
	}
	cache := &countsCache{states: make(map[string]*countsSourceState), now: now, collector: collector}
	if _, err := rand.Read(cache.key[:]); err != nil {
		return nil, err
	}
	return cache, nil
}

func (cache *countsCache) prepareSource(client *UpstreamClient, identity *ClientIdentityService, proxyURL string) countsSourceSpec {
	return prepareCountsSource(client, identity, proxyURL, cache.key)
}

func (cache *countsCache) nextGenerationLocked() uint64 {
	cache.sequence++
	return cache.sequence
}

// Call after authoritative source publication, under the App lifecycle gate.
// No network or goroutine starts here. Replaced instances keep valid data but
// invalidate/cancel the old work lease. Cooldowns survive same-ID config edits.
func (cache *countsCache) syncSource(source countsSourceSpec) bool {
	if source.Client == nil || source.Client.ID == "" || source.Client.countsAuthState().Retired {
		return false
	}
	source.IdentityHeaders = cloneHeader(source.IdentityHeaders)
	id := source.Client.ID
	cache.mu.Lock()
	defer cache.mu.Unlock()
	if cache.closed {
		return false
	}
	state := cache.states[id]
	if state == nil {
		state = &countsSourceState{
			spec: source, incarnation: cache.nextGenerationLocked(),
			dataGeneration: cache.nextGenerationLocked(), workGeneration: cache.nextGenerationLocked(),
		}
		cache.states[id] = state
		return true
	}
	dataChanged := state.spec.DataSignature != source.DataSignature
	workChanged := dataChanged || state.spec.Client != source.Client || state.spec.WorkSignature != source.WorkSignature
	if !workChanged {
		return false
	}
	if dataChanged {
		state.dataGeneration = cache.nextGenerationLocked()
		state.snapshot, state.hasSnapshot = countsSnapshot{}, false
	}
	state.workGeneration = cache.nextGenerationLocked()
	state.spec = source
	if state.inFlight != nil {
		state.inFlight.cancel()
		// The canceled owner retains the slot until completion, so a replacement
		// cannot start a second same-source round while the old one is unwinding.
		state.pending = true
	}
	return true
}

// Source deletion (not unbinding a user) is the only removal operation. The
// monotonic incarnation prevents an old completion from reviving a readded ID.
func (cache *countsCache) removeSource(id string) {
	cache.mu.Lock()
	defer cache.mu.Unlock()
	if state := cache.states[id]; state != nil {
		if state.inFlight != nil {
			state.inFlight.cancel()
		}
		delete(cache.states, id)
	}
}

func (cache *countsCache) requestRefresh(id string) bool {
	cache.mu.Lock()
	defer cache.mu.Unlock()
	if cache.closed {
		return false
	}
	state := cache.states[id]
	if state == nil {
		return false
	}
	if flight := state.inFlight; flight != nil && flight.workGeneration == state.workGeneration {
		// Extra binding/hour triggers join this round, not a new completion-time
		// queue entry. Invalidated old work, however, must leave one new intent.
		return true
	}
	state.pending = true
	return true
}

func (cache *countsCache) readLocked(id string) countsCacheView {
	state := cache.states[id]
	if cache.closed {
		return countsCacheView{LastErrorClass: countsClosed}
	}
	if state == nil {
		return countsCacheView{LastErrorClass: countsChanged}
	}
	auth := state.spec.Client.countsAuthState()
	view := countsCacheView{
		Online:   auth.Online && !auth.Retired && auth.Auth.UserID != "" && auth.Auth.AccessToken != "",
		Ready:    state.spec.current(),
		InFlight: state.inFlight != nil, Pending: state.pending,
		LastAttempt: state.lastAttempt, LastErrorClass: state.lastErrorClass,
		NextAllowedAt:  state.nextAllowedAt,
		DataGeneration: state.dataGeneration, WorkGeneration: state.workGeneration,
	}
	// A same-account token/identity change does not discard success. A changed
	// real upstream user or retired object cannot provide a current snapshot.
	if state.hasSnapshot && !auth.Retired && auth.AccountCurrent && auth.Auth.UserID == state.spec.Auth.UserID &&
		state.snapshot.DataGeneration == state.dataGeneration {
		view.Snapshot, view.HasSnapshot = state.snapshot, true
	}
	return view
}

func (cache *countsCache) read(id string) countsCacheView {
	cache.mu.RLock()
	defer cache.mu.RUnlock()
	return cache.readLocked(id)
}

// refresh synchronously runs one owned round or joins an existing one. Its
// boolean reports ownership for a worker's completion/wakeup accounting.
// It never configures a source: a stale caller cannot republish old credentials.
func (cache *countsCache) refresh(ctx context.Context, id string) (countsRoundResult, bool) {
	return cache.refreshWork(ctx, id, 0, 0)
}

func (cache *countsCache) refreshWork(ctx context.Context, id string, incarnation, workGeneration uint64) (countsRoundResult, bool) {
	if ctx == nil {
		ctx = context.Background()
	}
	if ctx.Err() != nil {
		return countsRoundResult{Class: countsCanceled}, false
	}
	cache.mu.Lock()
	if cache.closed {
		cache.mu.Unlock()
		return countsRoundResult{Class: countsClosed}, false
	}
	state := cache.states[id]
	if state == nil {
		cache.mu.Unlock()
		return countsRoundResult{Class: countsChanged}, false
	}
	if incarnation != 0 && (state.incarnation != incarnation || state.workGeneration != workGeneration) {
		cache.mu.Unlock()
		return countsRoundResult{Class: countsChanged}, false
	}
	if flight := state.inFlight; flight != nil {
		done := flight.done
		cache.mu.Unlock()
		select {
		case <-ctx.Done():
			return countsRoundResult{Class: countsCanceled}, false
		case <-done:
			return flight.result, false
		}
	}
	now := cache.now()
	if state.nextAllowedAt.After(now) {
		state.pending = true
		result := countsRoundResult{Class: countsLimited,
			Wait: countsWait{Limited: true, Until: state.nextAllowedAt}}
		cache.mu.Unlock()
		return result, false
	}
	if !state.spec.current() {
		state.pending, state.lastErrorClass = true, countsNotReady
		cache.mu.Unlock()
		return countsRoundResult{Class: countsNotReady}, false
	}
	roundCtx, cancel := context.WithTimeout(ctx, countsRoundTimeout)
	flight := &countsFlight{
		incarnation: state.incarnation, dataGeneration: state.dataGeneration,
		workGeneration: state.workGeneration, roundID: cache.nextGenerationLocked(),
		source: state.spec, done: make(chan struct{}), cancel: cancel,
	}
	state.inFlight, state.pending, state.lastAttempt = flight, false, now
	cache.running.Add(1) // Serialized with closed, before Close may Wait.
	cache.mu.Unlock()
	defer cache.running.Done()
	defer cancel()

	result := cache.collector.collect(roundCtx, flight.source)
	cache.finish(id, flight, roundCtx, result)
	return flight.result, true
}

func (cache *countsCache) applyWaitLocked(state *countsSourceState, wait countsWait) {
	if !wait.Limited {
		return
	}
	until := wait.Until
	if wait.NeedsFallback || !wait.HasServerWait {
		if state.limitedFailures < 5 {
			state.limitedFailures++
		}
		delay := 5 * time.Minute * time.Duration(1<<(state.limitedFailures-1))
		if delay > time.Hour {
			delay = time.Hour
		}
		fallback := cache.now().Add(delay)
		if fallback.After(until) {
			until = fallback
		}
	} else {
		state.limitedFailures = 0
	}
	if until.After(state.nextAllowedAt) {
		state.nextAllowedAt = until
	}
	state.pending = true
}

func (cache *countsCache) finish(id string, flight *countsFlight, ctx context.Context, result countsRoundResult) {
	offlinePublished := false
	// This outer defer runs after the lifecycle/cache/client/identity gates release.
	defer func() {
		if offlinePublished {
			flight.source.Client.notifyCountsState()
		}
	}()

	allowed := true
	if cache.publicationGate != nil {
		release, valid := cache.publicationGate(ctx, id, flight.source)
		allowed = valid
		if release != nil {
			defer release()
		}
	}
	cache.mu.Lock()
	defer cache.mu.Unlock()
	state := cache.states[id]
	validIncarnation := !cache.closed && state != nil && state.incarnation == flight.incarnation
	if validIncarnation {
		// A received wait applies to this source even when its account/connection
		// changed while the body/check was being processed. Data remains fenced.
		cache.applyWaitLocked(state, result.Wait)
	}
	validLease := validIncarnation && state.inFlight == flight &&
		state.dataGeneration == flight.dataGeneration && state.workGeneration == flight.workGeneration
	current := false
	if allowed && validLease && ctx.Err() == nil {
		release, valid := flight.source.lockPublication()
		current = valid && ctx.Err() == nil
		if release != nil {
			defer release()
		}
	}
	if current {
		state.lastErrorClass = result.Class
		if result.Success && result.Value.validSource() {
			state.snapshot = countsSnapshot{Value: result.Value, SucceededAt: cache.now(),
				DataGeneration: state.dataGeneration}
			state.hasSnapshot = true
			state.limitedFailures = 0
			state.nextAllowedAt = time.Time{}
			state.lastErrorClass = countsOK
		} else if result.Success {
			result.Success, result.Class, state.lastErrorClass = false, countsPayload, countsPayload
		}
		if result.Reachability == countsReachabilityOffline {
			if !flight.source.Client.publishCountsOfflineLocked(flight.source.Auth, flight.source.Observation) {
				result.Success, result.Class, state.lastErrorClass = false, countsChanged, countsChanged
			} else {
				offlinePublished = true
			}
		}
	} else {
		result.Success = false
		if ctx.Err() != nil {
			result.Class = countsCanceled
		} else {
			result.Class = countsChanged
		}
	}
	if validIncarnation && state.inFlight == flight {
		state.inFlight = nil
		// No implicit retry for ordinary failure. A known cooldown or changed
		// source leaves only the already recorded pending intent.
	}
	flight.result = result
	close(flight.done)
}

func (cache *countsCache) pruneSources(configured map[string]bool) {
	cache.mu.Lock()
	defer cache.mu.Unlock()
	for id, state := range cache.states {
		if configured[id] {
			continue
		}
		if state.inFlight != nil {
			state.inFlight.cancel()
		}
		delete(cache.states, id)
	}
}

// No successful data is destroyed by a pending lifecycle cleanup. Cancel only
// live leases and retain one replacement intent, evaluated after recovery.
func (cache *countsCache) cancelWork() {
	cache.mu.Lock()
	defer cache.mu.Unlock()
	if cache.closed {
		return
	}
	for _, state := range cache.states {
		if state.inFlight == nil {
			continue
		}
		state.workGeneration = cache.nextGenerationLocked()
		state.inFlight.cancel()
		state.pending = true
	}
}

func (cache *countsCache) close() {
	cache.mu.Lock()
	cache.closed = true
	for _, state := range cache.states {
		if state.inFlight != nil {
			state.inFlight.cancel()
		}
	}
	cache.mu.Unlock()
	cache.running.Wait()
	cache.mu.Lock()
	cache.states = make(map[string]*countsSourceState)
	cache.mu.Unlock()
}
