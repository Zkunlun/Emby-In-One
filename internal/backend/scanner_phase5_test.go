package backend

import (
	"errors"
	"net/http"
	"testing"
	"time"
)

func TestPhase5ADefaultDisabledAndDurableSchema(t *testing.T) {
	withTask6HTTPFixture(t, nil, nil, func(f *task6HTTPFixture) {
		if f.app.Scanner == nil {
			t.Fatal("scanner control not initialized")
		}
		admin := loginTokenAs(t, f.handler, "admin", "secret")
		rr := doAuthJSON(t, f.handler, http.MethodGet, "/admin/api/scanner/status", nil, admin)
		if rr.Code != 200 {
			t.Fatalf("scanner status=%d %s", rr.Code, rr.Body.String())
		}
		body := task6HTTPJSON(t, rr)
		if body["scanEnabled"] != false || body["executorAvailable"] != false {
			t.Fatalf("scanner must be off and worker absent: %v", body)
		}
		for _, up := range f.upstream {
			if n := up.count("/Items"); n != 0 {
				t.Fatalf("scanner default startup issued requests to %s: %d", up.label, n)
			}
		}
		if n := task6SQLCount(t, f.app.IDStore.db, "scanner_sources"); n != 0 {
			t.Fatalf("unexpected scan allowlist %d", n)
		}
		if n := task6SQLCount(t, f.app.IDStore.db, "scanner_runs"); n != 0 {
			t.Fatalf("unexpected active scan %d", n)
		}
		if err := initScannerSchema(f.app.IDStore.db); err != nil {
			t.Fatalf("idempotent migration %v", err)
		}
	})
}

func TestPhase5BControlTransitionsAndCheckpoint(t *testing.T) {
	withTask6HTTPFixture(t, nil, nil, func(f *task6HTTPFixture) {
		admin := loginTokenAs(t, f.handler, "admin", "secret")
		source := "server-a"
		path := "/admin/api/scanner/upstreams/" + source
		command := func(action string) *scanRun {
			t.Helper()
			rr := doAuthJSON(t, f.handler, http.MethodPost, path+"/commands/"+action, nil, admin)
			if rr.Code != 200 {
				t.Fatalf("%s: %d %s", action, rr.Code, rr.Body.String())
			}
			active, err := f.app.Scanner.runLocked(source, false)
			if err != nil || active == nil {
				t.Fatalf("read %s: %v", action, err)
			}
			return active
		}
		if rr := doAuthJSON(t, f.handler, http.MethodPost, path+"/commands/start", nil, admin); rr.Code != 403 {
			t.Fatalf("start without permission=%d", rr.Code)
		}
		if rr := doAuthJSON(t, f.handler, http.MethodPut, path, map[string]any{"allowScan": true}, admin); rr.Code != 200 {
			t.Fatalf("allow scan %d %s", rr.Code, rr.Body.String())
		}
		if rr := doAuthJSON(t, f.handler, http.MethodPost, path+"/commands/start", nil, admin); rr.Code != 403 {
			t.Fatalf("per source only wrongly starts %d", rr.Code)
		}
		if rr := doAuthJSON(t, f.handler, http.MethodPut, "/admin/api/scanner/settings", map[string]any{"scanEnabled": true}, admin); rr.Code != 200 {
			t.Fatalf("global enable %d %s", rr.Code, rr.Body.String())
		}
		first := command("start")
		if first.State != scanQueued || first.Type != "full" {
			t.Fatalf("initial run %+v", first)
		}
		if rr := doAuthJSON(t, f.handler, http.MethodPost, path+"/commands/start", nil, admin); rr.Code != 409 {
			t.Fatalf("duplicate run status %d", rr.Code)
		}
		paused := command("pause")
		if paused.ID != first.ID || paused.State != scanPausedAdmin {
			t.Fatalf("pause lost checkpoint run %v", paused)
		}
		pausedAgain := command("pause")
		if pausedAgain.Revision != paused.Revision {
			t.Fatal("idempotent pause bumped revision")
		}
		resumed := command("resume")
		if resumed.ID != first.ID || resumed.State != scanQueued {
			t.Fatal("resume created new run")
		}
		if err := f.app.scannerSetRunLibraries(source, first.ID, []string{"library-a"}); err != nil {
			t.Fatal(err)
		}
		if err := f.app.scannerRecordCommittedPage(source, first.ID, "library-a", 0, 60, 60); err != nil {
			t.Fatal(err)
		}
		if err := f.app.scannerRecordCommittedPage(source, first.ID, "library-a", 0, 60, 60); err != nil {
			t.Fatal("repeat page", err)
		}
		if err := f.app.scannerRecordCommittedPage(source, first.ID, "library-a", 0, 120, 60); !errors.Is(err, errScanTransition) {
			t.Fatal("out-of-order checkpoint accepted", err)
		}
		current, err := f.app.Scanner.runLocked(source, true)
		if err != nil || current.Pages != 1 {
			t.Fatalf("checkpoint double counted %+v %v", current, err)
		}
		stop := command("stop")
		if stop.State != scanStopped {
			t.Fatalf("stop %+v", stop)
		}
		stopAgain := command("stop")
		if stopAgain.Revision != stop.Revision {
			t.Fatal("repeated stop changed revision")
		}
		second := command("start")
		if second.ID == first.ID || second.Type != "full" {
			t.Fatalf("new run did not start after stop %+v", second)
		}
		if n := task6SQLCount(t, f.app.IDStore.db, "scanner_checkpoints"); n != 1 {
			t.Fatalf("stopped checkpoint lost: %d", n)
		}
		if n := f.upstream[0].count("/Items"); n != 0 {
			t.Fatalf("phase5 commands issued scanner HTTP: %d", n)
		}
	})
}

func TestPhase5CPermissionPauseAndAdminSecurity(t *testing.T) {
	withTask6HTTPFixture(t, nil, nil, func(f *task6HTTPFixture) {
		user := f.token
		admin := loginTokenAs(t, f.handler, "admin", "secret")
		if rr := doAuthJSON(t, f.handler, http.MethodPut, "/admin/api/scanner/settings", map[string]any{"scanEnabled": true}, user); rr.Code != 403 {
			t.Fatalf("regular user changed scanner config: %d", rr.Code)
		}
		doAuthJSON(t, f.handler, http.MethodPut, "/admin/api/scanner/settings", map[string]any{"scanEnabled": true}, admin)
		doAuthJSON(t, f.handler, http.MethodPut, "/admin/api/scanner/upstreams/server-b", map[string]any{"allowScan": true}, admin)
		r := doAuthJSON(t, f.handler, http.MethodPost, "/admin/api/scanner/upstreams/server-b/commands/start", nil, admin)
		if r.Code != 200 {
			t.Fatalf("start %d %s", r.Code, r.Body.String())
		}
		doAuthJSON(t, f.handler, http.MethodPut, "/admin/api/scanner/settings", map[string]any{"scanEnabled": false}, admin)
		state, _ := f.app.Scanner.runLocked("server-b", true)
		if state == nil || state.State != scanPausedPermission {
			t.Fatalf("disable did not pause: %+v", state)
		}
		doAuthJSON(t, f.handler, http.MethodPut, "/admin/api/scanner/settings", map[string]any{"scanEnabled": true}, admin)
		state, _ = f.app.Scanner.runLocked("server-b", true)
		if state.State != scanPausedPermission {
			t.Fatalf("enable resumed without explicit Resume: %+v", state)
		}
		r = doAuthJSON(t, f.handler, http.MethodPost, "/admin/api/scanner/upstreams/server-b/commands/resume", nil, admin)
		if r.Code != 200 {
			t.Fatalf("resume %d %s", r.Code, r.Body.String())
		}
		state, _ = f.app.Scanner.runLocked("server-b", true)
		if state.State != scanQueued {
			t.Fatal("resume did not queue", state)
		}
	})
}

func TestPhase5DDailySlotShanghaiIdempotent(t *testing.T) {
	withTask6HTTPFixture(t, nil, nil, func(f *task6HTTPFixture) {
		s := f.app.Scanner
		s.now = func() time.Time { return time.Date(2026, 10, 11, 5, 0, 0, 0, time.FixedZone("CST", 8*3600)) }
		if err := f.app.setScannerGlobal(true); err != nil {
			t.Fatal(err)
		}
		if err := f.app.setScannerSource("server-a", true); err != nil {
			t.Fatal(err)
		}
		// With initial Full incomplete, no auto delta is allowed.
		moment := time.Date(2026, 10, 11, 5, 0, 0, 0, time.FixedZone("CST", 8*3600))
		reason, err := f.app.scannerDailySlot("server-a", moment)
		if err != nil || reason != "skipped_initial_pending" {
			t.Fatalf("initial slot %q %v", reason, err)
		}
		reason, err = f.app.scannerDailySlot("server-a", moment)
		if err != nil || reason != "already_consumed" {
			t.Fatalf("duplicate slot %q %v", reason, err)
		}
		if err := s.db.writeParams(`UPDATE scanner_sources SET initial_full_completed=1 WHERE source_id=?`, "server-a"); err != nil {
			t.Fatal(err)
		}
		tomorrow := moment.Add(24 * time.Hour)
		reason, err = f.app.scannerDailySlot("server-a", tomorrow)
		if err != nil || reason != "queued" {
			t.Fatalf("delta slot %q %v", reason, err)
		}
		reason, err = f.app.scannerDailySlot("server-a", tomorrow)
		if err != nil || reason != "already_consumed" {
			t.Fatalf("duplicate delta slot %q %v", reason, err)
		}
		if n := task6SQLCount(t, s.db, "scanner_runs"); n != 1 {
			t.Fatalf("double auto run: %d", n)
		}
		if n := task6SQLCount(t, s.db, "scanner_schedule_slots"); n != 2 {
			t.Fatalf("slots=%d", n)
		}
		reason, err = f.app.scannerDailySlot("server-b", moment.Add(2*time.Hour))
		if err != nil || reason != "outside_slot" {
			t.Fatalf("missed slot caught up: %s %v", reason, err)
		}
		if n := f.upstream[0].count("/Items"); n != 0 {
			t.Fatal("Phase5 scheduler fetched upstream items")
		}
	})
}

func TestPhase5EUpstreamDeleteCleansScannerRows(t *testing.T) {
	withTask6HTTPFixture(t, nil, nil, func(f *task6HTTPFixture) {
		admin := loginTokenAs(t, f.handler, "admin", "secret")
		if err := f.app.setScannerGlobal(true); err != nil {
			t.Fatal(err)
		}
		if err := f.app.setScannerSource("server-b", true); err != nil {
			t.Fatal(err)
		}
		run, err := f.app.scannerCommand("server-b", "start")
		if err != nil {
			t.Fatal(err)
		}
		if err := f.app.scannerSetRunLibraries("server-b", run.ID, []string{"b-library"}); err != nil {
			t.Fatal(err)
		}
		if err := f.app.scannerRecordCommittedPage("server-b", run.ID, "b-library", 0, 60, 40); err != nil {
			t.Fatal(err)
		}
		response := doAuthJSON(t, f.handler, http.MethodDelete, "/admin/api/upstream/server-b", nil, admin)
		if response.Code != 200 {
			t.Fatalf("delete %d %s", response.Code, response.Body.String())
		}
		for _, table := range []string{"scanner_sources", "scanner_runs", "scanner_checkpoints", "scanner_schedule_slots"} {
			if n := task6SQLCount(t, f.app.IDStore.db, table); n != 0 {
				t.Fatalf("%s survived source removal: %d", table, n)
			}
		}
		if err := f.app.scannerRecordCommittedPage("server-b", run.ID, "b-library", 60, 120, 40); err == nil {
			t.Fatal("stale page resurrected scanner state")
		}
	})
}
