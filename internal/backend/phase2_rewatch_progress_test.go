package backend

import (
	"net/http"
	"reflect"
	"testing"
)

// A past completion remains true even while a new viewing has a nonzero resume position.
func TestPhase2RewatchPlaybackPersistsProgressAfterPlayed(t *testing.T) {
	ws := newTestWatchStore(t)
	meta := &WatchProgress{ProxyUserID: "user", VirtualItemID: "movie", ServerID: "server", OriginalItemID: "original", ItemType: "Movie"}
	if err := ws.RecordProgress(meta); err != nil {
		t.Fatal(err)
	}
	if err := ws.MarkPlayed("user", "movie", true); err != nil {
		t.Fatal(err)
	}
	if err := ws.SetFavorite("user", "movie", true); err != nil {
		t.Fatal(err)
	}
	record := func(kind playbackWatchEventKind, ticks int64) {
		t.Helper()
		if err := ws.RecordPlaybackProgress(meta, playbackWatchEvent{
			Kind:     kind,
			Source:   playbackWatchSource{ServerID: "server", OriginalItemID: "original", MediaSourceID: "version-a"},
			Position: validPlaybackTicks(ticks),
			Runtime:  validPlaybackTicks(1000),
		}); err != nil {
			t.Fatal(err)
		}
	}
	check := func(played bool, pos int64) {
		t.Helper()
		got := ws.GetProgress("user", "movie")
		if got == nil || got.Played != played || got.PositionTicks != pos || !got.IsFavorite {
			t.Fatalf("played/position/favorite=%+v, want played=%v position=%d favorite=true", got, played, pos)
		}
	}
	record(playbackWatchStarted, 0)
	record(playbackWatchProgress, 300)
	check(true, 300)
	record(playbackWatchStopped, 300)
	check(true, 300)

	record(playbackWatchStarted, 0)
	record(playbackWatchProgress, 950)
	check(true, 950) // No completion on Progress, even during a rewatch.
	record(playbackWatchStopped, 950)
	check(true, 0) // Completed rewatch clears this viewing's resume position only.
}

func TestPhase2PlayedRewatchIsResumableAndUserDataConsistent(t *testing.T) {
	ws := newTestWatchStore(t)
	item := &WatchProgress{ProxyUserID: "user", VirtualItemID: "movie", ServerID: "server", OriginalItemID: "original", ItemType: "Movie", PositionTicks: 300, RuntimeTicks: 1000, Played: true}
	if err := ws.RecordProgress(item); err != nil {
		t.Fatal(err)
	}
	resume, err := ws.GetResumeItems("user", 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(resume) != 1 || resume[0].VirtualItemID != "movie" {
		t.Fatalf("resume=%+v, want previously played movie", resume)
	}
	rows, err := ws.GetResumableItems("user")
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != 1 || rows[0].VirtualItemID != "movie" {
		t.Fatalf("resumable=%+v, want previously played movie", rows)
	}
	if !satisfiesUserState(*item, userStateFilter{resumable: true}) || !satisfiesCandidateUserState(*item, true, userStateFilter{resumable: true}) {
		t.Fatal("IsResumable excluded a rewatch with recorded position")
	}
	ud := map[string]any{}
	applyUserDataState(ud, item)
	if ud["Played"] != true || ud["PlaybackPositionTicks"] != int64(300) || ud["PlayedPercentage"] != float64(30) {
		t.Fatalf("rewatch UserData=%#v", ud)
	}
	item.PositionTicks = 0
	ud = map[string]any{}
	applyUserDataState(ud, item)
	if ud["Played"] != true || ud["PlaybackPositionTicks"] != int64(0) || ud["PlayedPercentage"] != float64(100) {
		t.Fatalf("completed UserData=%#v", ud)
	}
}

func TestPhase2VisibleResumeIncludesPreviouslyPlayedInProgress(t *testing.T) {
	withUnifiedWatchFixture(t, func(f *unifiedWatchFixture) {
		if err := f.app.WatchStore.RecordProgress(&WatchProgress{
			ProxyUserID: f.info.UserID, VirtualItemID: f.item,
			ServerID: "server-a", OriginalItemID: "item-a",
			ItemType: "Movie", PositionTicks: 320, RuntimeTicks: 1000, Played: true,
		}); err != nil {
			t.Fatal(err)
		}
		scope := mediaAccessScope{userID: f.info.UserID, serverIDs: []string{"server-a", "server-b"}}
		check := func(label string, rows []WatchProgress, err error) {
			t.Helper()
			if err != nil || len(rows) != 1 || rows[0].VirtualItemID != f.item || !rows[0].Played || rows[0].PositionTicks != 320 {
				t.Fatalf("%s: rows=%+v err=%v", label, rows, err)
			}
		}
		rows, err := f.app.WatchStore.GetVisibleResumableItems(scope)
		check("visible resumable", rows, err)
		rows, err = f.app.WatchStore.GetVisibleResumeCandidates(scope, "")
		check("resume candidates", rows, err)
		rows, err = f.app.WatchStore.GetVisibleResumeItems(scope, 10)
		check("resume page", rows, err)
	})
}

func TestPhase2ResumableFilterReturnsPlayedRewatchWithUserData(t *testing.T) {
	stub := newFilterStub(t, []map[string]any{
		filterStubItem("movie-a", "Alpha", "lib-1", 2001, filterStubUserData(false, false, 0)),
		filterStubItem("movie-b", "Bravo", "lib-1", 2002, filterStubUserData(false, false, 0)),
	})
	withTempAppConfig(t, singleUpstreamConfig(stub.server.URL), func(app *App, handler http.Handler) {
		userToken := createRegularUser(t, handler)
		info := app.Auth.ValidateToken(userToken)
		if info == nil {
			t.Fatal("missing test user")
		}
		serverID := app.Upstream.Clients()[0].ID
		virtualID := app.IDStore.GetOrCreateVirtualID("movie-b", serverID)
		if err := app.WatchStore.RecordProgress(&WatchProgress{
			ProxyUserID: info.UserID, VirtualItemID: virtualID,
			ServerID: serverID, OriginalItemID: "movie-b",
			ItemType: "Movie", PositionTicks: 300, RuntimeTicks: 1000, Played: true,
		}); err != nil {
			t.Fatal(err)
		}
		for _, filter := range []string{"IsResumable", "IsPlayed"} {
			rr := doJSONRequest(t, handler, http.MethodGet,
				"/Users/"+app.Auth.ProxyUserID()+"/Items?Filters="+filter,
				nil, userToken)
			if rr.Code != http.StatusOK {
				t.Fatalf("%s status=%d body=%s", filter, rr.Code, rr.Body.String())
			}
			if names := itemNames(t, rr.Body.Bytes()); !reflect.DeepEqual(names, []string{"Bravo"}) {
				t.Fatalf("%s items=%v", filter, names)
			}
			ud := itemUserDataOf(t, rr.Body.Bytes(), "Bravo")
			pos, _ := numericInt64(ud["PlaybackPositionTicks"])
			if ud["Played"] != true || pos != 300 || ud["PlayedPercentage"] != float64(30) {
				t.Fatalf("%s rewatch state=%#v", filter, ud)
			}
		}
	})
}
