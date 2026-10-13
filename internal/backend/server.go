package backend

import (
	"context"
	"errors"
	"net/http"
	"os"
	"os/signal"
	"sync"
	"syscall"
	"time"
)

// App is the main application struct holding all shared dependencies.
type App struct {
	Version         string
	ConfigStore     *ConfigStore
	Logger          *Logger
	IDStore         *IDStore
	Identity        *ClientIdentityService
	Auth            *AuthManager
	Upstream        *UpstreamPool
	UserStore       *UserStore
	WatchStore      *WatchStore
	HiddenLibraries *HiddenLibraryStore
	libraryCache    *upstreamLibraryCache
	mediaCounts     *mediaCountsService
	PlaybackLimiter *PlaybackLimiter
	playbackRoutes  *playbackRouteStore
	watchPlayback   playbackWatchCache
	loginLimiter    loginRateLimiter
	noticeThrottle  noticeThrottle
	// grantCapacityMu serializes full-config admin writes with authorization-grant
	// mutations so a stale Config snapshot cannot restore an old maxConcurrent or
	// recreate a grant for an upstream that was just deleted.
	grantCapacityMu sync.Mutex
	// watchLifecycleMu coordinates request-scoped authorization/state reads with
	// binding and deletion publication. Never hold it during upstream I/O.
	watchLifecycleMu sync.RWMutex
	lifecyclePending bool // Protected by watchLifecycleMu; unresolved cleanup denies regular access.
}

func NewApp() (*App, error) {
 configStore, err := LoadConfigStore()
 if err != nil { return nil, err }
 cfg := configStore.Snapshot()
 logger := NewLogger(LogConfig{DataDir: cfg.DataDir})
 logTimeoutNotice(logger, cfg.Timeouts)
 upstreamIDs := make([]string, len(cfg.Upstream))
 for i, source := range cfg.Upstream { upstreamIDs[i] = source.ID }
 idStore, err := NewIDStore(cfg.DataDir, logger, upstreamIDs...)
 if err != nil { _ = logger.Close(); return nil, err }
 success := false
 defer func() { if !success { _ = idStore.Close(); _ = logger.Close() } }()
 var users *UserStore
 var watch *WatchStore
 var hidden *HiddenLibraryStore
 if db := idStore.DB(); db != nil {
  users, err = NewUserStore(db, logger)
  if err != nil { return nil, err }
  watch, err = NewWatchStore(db, logger)
  if err != nil { return nil, err }
  hidden, err = NewHiddenLibraryStore(db, logger)
  if err != nil { return nil, err }
 }
 identity := NewClientIdentityServiceFromDetectedConfig()
 auth, err := NewAuthManager(configStore, identity, logger, users)
 if err != nil { return nil, err }
 app := &App{ConfigStore: configStore, Logger: logger, IDStore: idStore,
  Identity: identity, Auth: auth, UserStore: users, WatchStore: watch,
  HiddenLibraries: hidden, libraryCache: newUpstreamLibraryCache(),
  PlaybackLimiter: NewPlaybackLimiter(), playbackRoutes: newPlaybackRouteStore()}
 app.watchLifecycleMu.Lock()
 app.publishConfiguredSourcesLocked(configStore.Snapshot())
 if idStore.DB() != nil { err = app.recoverWatchLifecycleLocked() }
 app.watchLifecycleMu.Unlock()
 if err != nil { return nil, err }
 // Recovery precedes identity migration, upstream login and HTTP authentication.
 if err := identity.migrateSourceOwnership(configStore.Snapshot().Upstream); err != nil { return nil, err }
 app.installIdentityLifecycle()
 app.Upstream = NewUpstreamPool(configStore.Snapshot(), logger)
 app.Upstream.LoginAll()
 counts, err := newMediaCountsService(app, realCountsScheduleClock(), nil)
 if err != nil { app.Upstream.stopHealthChecks(); return nil, err }
 app.mediaCounts = counts
 app.installCountsNotifications()
 counts.start()
 success = true
 return app, nil
}

// logTimeoutNotice warns when the per-operation timeouts are tighter than the API
// timeout. Those settings used to be ignored, so after an upgrade an upstream that
// logs in slowly can start failing with nothing else looking wrong.
func logTimeoutNotice(logger *Logger, timeouts TimeoutsConfig) {
	if logger == nil || (timeouts.Login >= timeouts.API && timeouts.HealthCheck >= timeouts.API) {
		return
	}
	logger.Infof("Timeouts are now enforced: login=%dms, healthCheck=%dms, api=%dms; "+
		"raise timeouts.login/healthCheck in the admin panel if an upstream connects slowly",
		timeouts.Login, timeouts.HealthCheck, timeouts.API)
}

func (a *App) Close() error {
	if a.mediaCounts != nil { a.mediaCounts.close() }
	if a.Upstream != nil {
		a.Upstream.stopHealthChecks()
	}
	if a.IDStore != nil {
		_ = a.IDStore.Close()
	}
	if a.Logger != nil {
		_ = a.Logger.Close()
	}
	return nil
}

func (a *App) Run() error {
	cfg := a.ConfigStore.Snapshot()
	server := &http.Server{
		Addr:              ":" + intToString(cfg.Server.Port),
		Handler:           a.Handler(),
		ReadHeaderTimeout: 10 * time.Second,
		// bodyLimitMiddleware caps a request body at 2 MB but not the time it may take to
		// arrive, and an idle connection used to be held forever. WriteTimeout stays unset
		// on purpose: the streaming endpoints write for as long as playback lasts.
		ReadTimeout: 60 * time.Second,
		IdleTimeout: 120 * time.Second,
	}

	// Start periodic stream URL eviction
	evictCtx, evictCancel := context.WithCancel(context.Background())
	defer evictCancel()
	go func() {
		ticker := time.NewTicker(30 * time.Minute)
		defer ticker.Stop()
		for {
			select {
			case <-evictCtx.Done():
				return
			case <-ticker.C:
				a.IDStore.evictExpiredStreamState()
				a.loginLimiter.cleanup()
				if a.PlaybackLimiter != nil {
					a.PlaybackLimiter.Cleanup()
				}
			}
		}
	}()

	// Graceful shutdown on SIGINT/SIGTERM
	shutdownCh := make(chan os.Signal, 1)
	signal.Notify(shutdownCh, os.Interrupt, syscall.SIGTERM)
	defer signal.Stop(shutdownCh)
	go func() {
		<-shutdownCh
		a.Logger.Infof("Shutdown signal received, draining connections...")
		evictCancel()
		if a.mediaCounts != nil { a.mediaCounts.close() }
		a.Upstream.stopHealthChecks()
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		_ = server.Shutdown(ctx)
	}()

	a.Logger.Infof("Go Emby-in-One listening on port %d", cfg.Server.Port)
	err := server.ListenAndServe()
	if errors.Is(err, http.ErrServerClosed) {
		return nil
	}
	return err
}
