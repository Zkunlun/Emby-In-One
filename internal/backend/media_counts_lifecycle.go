package backend

import (
	"context"
	"reflect"
	"time"
)

// RWMutex has no context-aware Lock. Contended background gates wait on a
// bounded timer, so shutdown and the worker's 90-second deadline can cancel
// before starting any network I/O. Uncontended acquisitions do not create timers.
func (a *App) lockCountsLifecycle(ctx context.Context) (func(), bool) {
	if ctx == nil {
		ctx = context.Background()
	}
	if ctx.Err() != nil {
		return nil, false
	}
	if a.watchLifecycleMu.TryRLock() {
		return a.watchLifecycleMu.RUnlock, true
	}
	ticker := time.NewTicker(50 * time.Millisecond)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return nil, false
		case <-ticker.C:
			if ctx.Err() != nil {
				return nil, false
			}
			if a.watchLifecycleMu.TryRLock() {
				return a.watchLifecycleMu.RUnlock, true
			}
		}
	}
}

// Requires the lifecycle gate. Configuration is authoritative, pool clients
// are live instances (never the presentation copies returned by snapshot()).
func (a *App) syncCountsSourcesLocked(cfg Config, refreshAll bool) {
	service := a.mediaCounts
	if service == nil || service.closed.Load() || a.Upstream == nil {
		return
	}
	service.cache.pruneSources(configuredSourceIDs(cfg))
	for _, configured := range cfg.Upstream {
		client := a.Upstream.ClientByID(configured.ID)
		if client == nil || !reflect.DeepEqual(client.Config, configured) {
			continue
		}
		proxyURL := ""
		if proxy := findProxy(cfg.Proxies, configured.ProxyID); proxy != nil {
			proxyURL = proxy.URL
		}
		source := service.cache.prepareSource(client, a.Identity, proxyURL)
		service.cache.mu.RLock()
		state := service.cache.states[configured.ID]
		request := state == nil
		if state != nil {
			previous := state.spec
			request = previous.DataSignature != source.DataSignature ||
				(!previous.Ready && source.Ready) ||
				(previous.Client == source.Client && previous.Observation != source.Observation && source.Ready) ||
				(client.Config.SpoofClient == "passthrough" && source.Ready &&
					!reflect.DeepEqual(previous.IdentityHeaders, source.IdentityHeaders))
		}
		service.cache.mu.RUnlock()
		service.cache.syncSource(source)
		if source.Ready && (refreshAll || request) {
			service.cache.requestRefresh(configured.ID)
		}
	}
}

func (a *App) countsPublicationGate(ctx context.Context, id string, source countsSourceSpec) (func(), bool) {
	release, locked := a.lockCountsLifecycle(ctx)
	if !locked {
		return nil, false
	}
	if a.lifecyclePending || a.mediaCounts == nil || a.mediaCounts.closed.Load() ||
		a.Upstream == nil || a.ConfigStore == nil || a.Upstream.ClientByID(id) != source.Client {
		return release, false
	}
	cfg := a.ConfigStore.Snapshot()
	for _, configured := range cfg.Upstream {
		if configured.ID != id {
			continue
		}
		if !reflect.DeepEqual(configured, source.Client.Config) {
			return release, false
		}
		proxyURL := ""
		if proxy := findProxy(cfg.Proxies, configured.ProxyID); proxy != nil {
			proxyURL = proxy.URL
		}
		current := a.mediaCounts.cache.prepareSource(source.Client, a.Identity, proxyURL)
		return release, current.DataSignature == source.DataSignature && current.WorkSignature == source.WorkSignature
	}
	return release, false
}

func (a *App) countsJobCurrent(ctx context.Context, job countsScheduledJob) bool {
	release, locked := a.lockCountsLifecycle(ctx)
	if !locked {
		return false
	}
	defer release()
	service := a.mediaCounts
	if service == nil || service.closed.Load() || a.lifecyclePending ||
		a.ConfigStore == nil || a.Upstream == nil ||
		!configuredSourceIDs(a.ConfigStore.Snapshot())[job.id] {
		return false
	}
	service.cache.mu.RLock()
	defer service.cache.mu.RUnlock()
	state := service.cache.states[job.id]
	return state != nil && state.incarnation == job.incarnation &&
		state.workGeneration == job.workGeneration && state.pending &&
		a.Upstream.ClientByID(job.id) == state.spec.Client && state.spec.current()
}

// Only the scheduler calls this. The dispatch map has at most two entries,
// including queued jobs; pending remains in the source cache, not per-user.
func (a *App) countsSchedule(now time.Time, active map[string]bool, refreshAll bool) ([]countsScheduledJob, time.Time) {
	service := a.mediaCounts
	if service == nil {
		return nil, time.Time{}
	}
	release, locked := a.lockCountsLifecycle(service.ctx)
	if !locked {
		return nil, time.Time{}
	}
	defer release()
	if service == nil || service.closed.Load() || a.ConfigStore == nil || a.lifecyclePending {
		return nil, time.Time{}
	}
	cfg := a.ConfigStore.Snapshot()
	a.syncCountsSourcesLocked(cfg, refreshAll)
	service.cache.mu.RLock()
	defer service.cache.mu.RUnlock()
	var jobs []countsScheduledJob
	var next time.Time
	seen := map[string]bool{}
	for _, configured := range cfg.Upstream {
		id := configured.ID
		if seen[id] {
			continue
		}
		seen[id] = true
		state := service.cache.states[id]
		if state == nil || !state.pending || state.inFlight != nil || active[id] ||
			!state.spec.current() {
			continue
		}
		if state.nextAllowedAt.After(now) {
			if next.IsZero() || state.nextAllowedAt.Before(next) {
				next = state.nextAllowedAt
			}
			continue
		}
		if len(active)+len(jobs) < countsWorkerLimit {
			jobs = append(jobs, countsScheduledJob{id, state.incarnation, state.workGeneration})
		}
	}
	return jobs, next
}

// Successful user publication only; empty/unchanged grants produce no request.
func (a *App) requestBoundCountsLocked(ids []string) {
	service := a.mediaCounts
	if service == nil || service.closed.Load() || a.lifecyclePending || a.ConfigStore == nil {
		return
	}
	a.syncCountsSourcesLocked(a.ConfigStore.Snapshot(), false)
	for _, id := range ids {
		if view := service.cache.read(id); view.Online {
			service.cache.requestRefresh(id)
		}
	}
	service.signal()
}

func countsSameBindings(first, second []string) bool {
	left, right := map[string]bool{}, map[string]bool{}
	for _, id := range first {
		left[id] = true
	}
	for _, id := range second {
		right[id] = true
	}
	return reflect.DeepEqual(left, right)
}

// Called inside the source/config commit boundary, including deletion before
// cleanup may fail. A deleted incarnation cannot survive until a later wake.
func (a *App) pruneCountsSourcesLocked(cfg Config) {
	if service := a.mediaCounts; service != nil {
		service.cache.pruneSources(configuredSourceIDs(cfg))
	}
}

func (a *App) suspendCountsWorkLocked() {
	if service := a.mediaCounts; service != nil {
		service.cache.cancelWork()
	}
}

func (a *App) resumeCountsSourcesLocked() {
	if service := a.mediaCounts; service != nil && !a.lifecyclePending {
		a.syncCountsSourcesLocked(a.ConfigStore.Snapshot(), false)
		service.signal()
	}
}

// Callbacks only signal; they do not acquire App/lifecycle/store/cache locks.
func (a *App) installCountsNotifications() {
	service := a.mediaCounts
	if service == nil {
		return
	}
	a.Upstream.setCountsStateListener(service.signal)
	if a.Identity != nil {
		a.Identity.setCountsIdentityListener(service.signal)
	}
}

func (c *UpstreamClient) notifyCountsState() {
	c.mu.RLock()
	listener := c.onCountsState
	c.mu.RUnlock()
	if listener != nil {
		listener()
	}
}

func (p *UpstreamPool) setCountsStateListener(listener func()) {
	p.mu.Lock()
	p.onCountsState = listener
	for _, client := range p.clients {
		if client == nil {
			continue
		}
		client.mu.Lock()
		client.onCountsState = listener
		client.mu.Unlock()
	}
	p.mu.Unlock()
}

func (s *ClientIdentityService) setCountsIdentityListener(listener func()) {
	s.mu.Lock()
	s.onCountsIdentity = listener
	s.mu.Unlock()
}

func (s *ClientIdentityService) notifyCountsIdentity() {
	if s == nil {
		return
	}
	s.mu.RLock()
	listener := s.onCountsIdentity
	s.mu.RUnlock()
	if listener != nil {
		listener()
	}
}
