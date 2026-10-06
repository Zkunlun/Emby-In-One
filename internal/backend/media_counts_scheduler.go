package backend

import (
	"context"
	"sync"
	"sync/atomic"
	"time"
)

const countsWorkerLimit = 2
const countsRefreshInterval = time.Hour

// Injectable clock edges let Phase4 prepare deterministic scheduling tests.
type countsScheduleClock struct {
	now    func() time.Time
	ticker func(time.Duration) (<-chan time.Time, func())
	timer  func(time.Duration) (<-chan time.Time, func())
}

func realCountsScheduleClock() countsScheduleClock {
	return countsScheduleClock{
		now: time.Now,
		ticker: func(d time.Duration) (<-chan time.Time, func()) {
			ticker := time.NewTicker(d)
			return ticker.C, ticker.Stop
		},
		timer: func(d time.Duration) (<-chan time.Time, func()) {
			timer := time.NewTimer(d)
			return timer.C, func() { timer.Stop() }
		},
	}
}

type countsScheduledJob struct {
	id             string
	incarnation    uint64
	workGeneration uint64
}

// Only run owns the dispatch set. Cache pending bits are the per-source queue;
// notices contain no credentials, source snapshots, user data or network work.
type mediaCountsService struct {
	app       *App
	cache     *countsCache
	clock     countsScheduleClock
	ctx       context.Context
	cancel    context.CancelFunc
	wake      chan struct{}
	jobs      chan countsScheduledJob
	completed chan countsScheduledJob
	running   sync.WaitGroup
	closeOnce sync.Once
	closed    atomic.Bool
}

func newMediaCountsService(app *App, clock countsScheduleClock, collector countsCollector) (*mediaCountsService, error) {
	realClock := realCountsScheduleClock()
	if clock.now == nil {
		clock.now = realClock.now
	}
	if clock.ticker == nil {
		clock.ticker = realClock.ticker
	}
	if clock.timer == nil {
		clock.timer = realClock.timer
	}

	cache, err := newCountsCache(clock.now, collector)
	if err != nil {
		return nil, err
	}
	ctx, cancel := context.WithCancel(context.Background())
	service := &mediaCountsService{
		app: app, cache: cache, clock: clock, ctx: ctx, cancel: cancel,
		wake: make(chan struct{}, 1), jobs: make(chan countsScheduledJob, countsWorkerLimit),
		completed: make(chan countsScheduledJob, countsWorkerLimit),
	}
	cache.publicationGate = app.countsPublicationGate
	return service, nil
}

// The App publishes this service and its gate before installing any listeners.
func (service *mediaCountsService) start() {
	service.running.Add(countsWorkerLimit + 1)
	for i := 0; i < countsWorkerLimit; i++ {
		go service.worker()
	}
	go service.run()
}

func (service *mediaCountsService) signal() {
	if service == nil || service.closed.Load() {
		return
	}
	select {
	case service.wake <- struct{}{}:
	default:
	}
}

func (service *mediaCountsService) close() {
	if service == nil {
		return
	}
	service.closeOnce.Do(func() {
		service.closed.Store(true)
		if service.app.Upstream != nil {
			service.app.Upstream.setCountsStateListener(nil)
		}
		if service.app.Identity != nil {
			service.app.Identity.setCountsIdentityListener(nil)
		}
		service.cancel()
		service.cache.close()
		service.running.Wait()
	})
}

func (service *mediaCountsService) worker() {
	defer service.running.Done()
	for {
		select {
		case <-service.ctx.Done():
			return
		case job := <-service.jobs:
			if service.ctx.Err() != nil {
				return
			}
			// Verify a queued lease against authoritative configuration. refreshWork
			// checks its generation again while acquiring the cache flight; neither
			// the gate nor any store lock is held during collector I/O.
			roundCtx, cancelRound := context.WithTimeout(service.ctx, countsRoundTimeout)
			if service.app.countsJobCurrent(roundCtx, job) {
				service.cache.refreshWork(roundCtx, job.id, job.incarnation, job.workGeneration)
			}
			cancelRound()
			select {
			case service.completed <- job:
			case <-service.ctx.Done():
				return
			}
		}
	}
}

func (service *mediaCountsService) run() {
	defer service.running.Done()
	ticks, stopTicker := service.clock.ticker(countsRefreshInterval)
	defer stopTicker()
	active := map[string]bool{}
	refreshAll := true // Startup initialization, after the source pool's login pass.
	for {
		if service.ctx.Err() != nil {
			return
		}
		jobs, next := service.app.countsSchedule(service.clock.now(), active, refreshAll)
		refreshAll = false
		for _, job := range jobs {
			if service.ctx.Err() != nil {
				return
			}
			active[job.id] = true
			select {
			case service.jobs <- job:
			case <-service.ctx.Done():
				return
			}
		}
		var deadline <-chan time.Time
		stopTimer := func() {}
		if !next.IsZero() {
			delay := next.Sub(service.clock.now())
			if delay < 0 {
				delay = 0
			}
			deadline, stopTimer = service.clock.timer(delay)
		}
		select {
		case <-service.ctx.Done():
			stopTimer()
			return
		case <-ticks:
			refreshAll = true
		case <-service.wake:
		case <-deadline:
		case job := <-service.completed:
			delete(active, job.id)
		}
		stopTimer()
	}
}
