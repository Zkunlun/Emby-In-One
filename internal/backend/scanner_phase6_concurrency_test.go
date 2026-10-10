package backend

import (
	"context"
	"net/http"
	"strings"
	"testing"
	"time"
)

func TestPhase6ConcurrentLanesRespectConfiguredCapacity(t *testing.T) {
	for _, cap := range []int{1, 2} {
		t.Run(map[int]string{1: "finite_one", 2: "finite_two"}[cap], func(t *testing.T) {
			rows := []map[string]any{}
			for i, lib := range []string{"library-a", "library-a-2"} {
				movie := task6HTTPMovie(lib)
				movie["ParentId"] = lib
				movie["Name"] = lib
				movie["ProviderIds"] = map[string]any{"Tmdb": lib}
				movie["ProductionYear"] = 2024 + i
				rows = append(rows, movie)
			}
			a := newPhase6MockSource(t, "a", rows)
			a.libraries = append(a.libraries, map[string]any{"Id": "library-a-2", "CollectionType": "tvshows"})
			a.pageStarted = make(chan struct{})
			a.pageContinue = make(chan struct{})
			b := newPhase6MockSource(t, "b", nil)
			config := phase1EDualPlaybackConfig(a.server.URL, b.server.URL)
			if cap == 2 {
				config = strings.ReplaceAll(config, "maxConcurrent: 1", "maxConcurrent: 2")
			}
			withTempAppConfig(t, config, func(app *App, _ http.Handler) {
				app.Scanner.wait = func(ctx context.Context, d time.Duration) error { return ctx.Err() }
				run := phase5Enable(t, app, "server-a")
				done := make(chan error, 1)
				go func() { done <- app.scannerRunFull(context.Background(), "server-a", run.ID) }()
				select {
				case <-a.pageStarted:
				case <-time.After(15 * time.Second):
					t.Fatal("scanner never requested a page")
				}
				deadline := time.Now().Add(3 * time.Second)
				for {
					a.mu.Lock()
					n := a.inFlight
					a.mu.Unlock()
					if n >= cap {
						break
					}
					if time.Now().After(deadline) {
						t.Fatalf("expected in-flight %d, got %d", cap, n)
					}
					time.Sleep(10 * time.Millisecond)
				}
				a.mu.Lock()
				observed := a.maxInFlight
				a.mu.Unlock()
				if observed != cap {
					t.Fatalf("MaxConcurrent %d violated, observed=%d", cap, observed)
				}
				close(a.pageContinue)
				select {
				case err := <-done:
					if err != nil {
						t.Fatal(err)
					}
				case <-time.After(10 * time.Second):
					t.Fatal("scanner did not drain in-flight requests")
				}
			})
		})
	}
}
