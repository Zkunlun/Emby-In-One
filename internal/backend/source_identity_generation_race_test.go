package backend

import (
	"errors"
	"testing"
	"time"
)

// A source can be deleted and recreated while an observation is waiting to
// acquire the lifecycle writer gate. The old observation must not be admitted
// merely because the same server ID has become configured again.
func TestPhase3EInFlightWriterCannotCrossDeleteRecreateGate(t *testing.T) {
	item := task6HTTPMovie("raced", task6HTTPSource("raced-source", 123))
	withTask6HTTPFixture(t, nil, []map[string]any{item}, func(f *task6HTTPFixture) {
		token := loginTokenAs(t, f.handler, "admin", "secret")
		old := phase3EHTTPContext(t, f.app, token)
		candidate := newMergeCandidate("server-b", item, nil, true)
		original := f.app.ConfigStore.Snapshot()
		source := original.Upstream[1]
		next := original
		next.Upstream = append([]UpstreamConfig(nil), original.Upstream[:1]...)
		started := make(chan struct{})
		finished := make(chan error, 1)
		f.app.watchLifecycleMu.Lock()
		go func() {
			close(started)
			_, err := f.app.mergeDiscovery().register(old, candidate, "", false)
			finished <- err
		}()
		<-started
		// The writer is waiting on the lifecycle gate; deletion and incarnation
		// publication occur as a single serialized update before it can commit.
		if err := f.app.deleteUpstreamLifecycleLocked(next, source); err != nil {
			f.app.watchLifecycleMu.Unlock()
			t.Fatal(err)
		}
		restored := f.app.ConfigStore.Snapshot()
		restored.Upstream = append(restored.Upstream, source)
		f.app.ConfigStore.Replace(restored)
		if err := f.app.ConfigStore.Save(); err != nil {
			f.app.watchLifecycleMu.Unlock()
			t.Fatal(err)
		}
		f.app.publishConfiguredSourcesLocked(restored)
		f.app.Upstream.Reload(restored)
		f.app.watchLifecycleMu.Unlock()
		select {
		case err := <-finished:
			if !errors.Is(err, errMediaAccessDenied) {
				t.Fatalf("stale writer crossed generation fence: %v", err)
			}
		case <-time.After(5 * time.Second):
			t.Fatal("writer did not resume after deletion gate")
		}
		if len(f.app.IDStore.MergeGroupsForItem(source.ID, "raced")) != 0 {
			t.Fatal("old in-flight observation revived deleted work")
		}
		// The authenticated fresh HTTP request is independently permitted.
		fresh := phase3EHTTPContext(t, f.app, token)
		if _, err := f.app.mergeDiscovery().register(fresh, candidate, "", false); err != nil {
			t.Fatal("fresh source request unexpectedly rejected", err)
		}
	})
}
