package backend

import (
	"net/http"
	"strconv"
	"testing"
)

func TestPhase7KAdminLifecycleCannotMutateRegularUserState(t *testing.T) {
	for _, path := range []string{"/Sessions/Playing", "/Sessions/Playing/Progress", "/Sessions/Playing/Stopped"} {
		t.Run(path, func(t *testing.T) {
			phase7KWithFixture(t, func(f *phase7KFixture) {
				before := f.Snapshot(t)
				admin := f.App.Auth.ValidateToken(f.AdminToken)
				if admin == nil || admin.Role != "admin" {
					t.Fatal("admin token missing")
				}
				rr := phase1ESessionPost(t, f.Handler, path, f.AdminToken, phase7KDeviceID, f.Body(0, 0, 700))
				phase7KAssertEmptySuccess(t, rr)
				f.AssertUnchangedExcept(t, before)
				f.AssertForwarded(t, 0, path, 700)
				if len(f.Upstreams[0].Requests()) != 1 || len(f.Upstreams[1].Requests()) != 0 {
					t.Fatal("admin lifecycle was forwarded to an unrelated upstream")
				}
				if p := f.App.WatchStore.GetProgress(admin.UserID, f.Items[0]); p != nil {
					t.Fatalf("admin lifecycle created local user progress: %+v", p)
				}
			})
		})
	}
}

func TestPhase7KUnauthorizedUpstreamLifecycleHasNoSideEffects(t *testing.T) {
	for _, path := range []string{"/Sessions/Playing", "/Sessions/Playing/Progress", "/Sessions/Playing/Stopped"} {
		t.Run(path, func(t *testing.T) {
			phase7KWithFixture(t, func(f *phase7KFixture) {
				limited := phase7KCreateUser(t, f, "phase7k-limited", []string{"server-a"})
				before := f.Snapshot(t)
				body := f.Body(0, 1, 700)
				body["UserId"] = limited.Info.UserID
				rr := phase1ESessionPost(t, f.Handler, path, limited.Token, phase7KDeviceID, body)
				if rr.Code != http.StatusForbidden {
					t.Fatalf("unauthorized upstream status=%d want=403 body=%s", rr.Code, rr.Body.String())
				}
				f.AssertUnchangedExcept(t, before)
				if len(f.Upstreams[0].Requests()) != 0 || len(f.Upstreams[1].Requests()) != 0 {
					t.Fatal("unauthorized session was reported upstream")
				}
				if p := f.App.WatchStore.GetProgress(limited.Info.UserID, f.Items[1]); p != nil {
					t.Fatalf("unauthorized session created local progress: %+v", p)
				}
			})
		})
	}
}

func TestPhase7KMatchingStoppedReleasesOnlyItsUserAndUpstream(t *testing.T) {
	for userIndex := 0; userIndex < 2; userIndex++ {
		for serverIndex := 0; serverIndex < 2; serverIndex++ {
			t.Run(strconv.Itoa(userIndex)+"/"+strconv.Itoa(serverIndex), func(t *testing.T) {
				phase7KWithFixture(t, func(f *phase7KFixture) {
					before := f.Snapshot(t)
					body := f.Body(userIndex, serverIndex, 700)
					body["UserId"] = f.Users[1-userIndex].Info.UserID
					rr := phase1ESessionPost(t, f.Handler, "/Sessions/Playing/Stopped", f.Users[userIndex].Token, phase7KDeviceID, body)
					phase7KAssertEmptySuccess(t, rr)
					f.AssertCommitted(t, before, userIndex, serverIndex, 700, true)
					f.AssertUnchangedExcept(t, before, f.Key(userIndex, serverIndex))
					f.AssertForwarded(t, serverIndex, "/Sessions/Playing/Stopped", 700)
					if len(f.Upstreams[serverIndex].Requests()) != 1 || len(f.Upstreams[1-serverIndex].Requests()) != 0 {
						t.Fatal("Stopped was reported to the wrong upstream")
					}
					if count := f.App.PlaybackLimiter.CountForServer(f.Servers[serverIndex]); count != 1 {
						t.Fatalf("target upstream leases=%d want=1", count)
					}
					if count := f.App.PlaybackLimiter.CountForServer(f.Servers[1-serverIndex]); count != 2 {
						t.Fatalf("other upstream leases=%d want=2", count)
					}
				})
			})
		}
	}
}

func TestPhase7KPlaybackRoutesRemainScopedToTheCurrentValidatedToken(t *testing.T) {
	phase7KWithFixture(t, func(f *phase7KFixture) {
		secondToken := loginTokenAs(t, f.Handler, "phase7k-alice", "password123")
		secondInfo := f.App.Auth.ValidateToken(secondToken)
		if secondInfo == nil || secondInfo.UserID != f.Users[0].Info.UserID || secondToken == f.Users[0].Token {
			t.Fatal("second login must issue a distinct token for the same user")
		}
		item := f.App.IDStore.GetOrCreateVirtualID("route-item-a", "server-a")
		f.App.IDStore.AssociateAdditionalInstance(item, "route-item-b", "server-b")
		f.App.playbackRoutes.Activate("token:"+f.Users[0].Token, item, "server-a", phase7KSessionID, f.Sessions[0])
		f.App.playbackRoutes.Activate("token:"+secondToken, item, "server-b", phase7KSessionID, f.Sessions[0])
		f.App.playbackRoutes.RememberMediaSource("token:"+secondToken, f.Media[1], "server-b", phase7KSessionID, f.Sessions[0])
		f.App.playbackRoutes.RememberMediaSourceItem("token:"+secondToken, f.Media[1], item, "server-b")
		// Deliberately disagree with the first token's route: the legacy global
		// item route must not override the current token's explicit mapping.
		f.App.IDStore.SetActiveStream(item, "server-b")
		before := f.Snapshot(t)
		for _, routeKind := range []string{"active", "media"} {
			for _, tc := range []struct {
				name       string
				token      string
				info       *tokenInfo
				wantServer string
			}{
				{name: "first token", token: f.Users[0].Token, info: f.Users[0].Info, wantServer: "server-a"},
				{name: "second token same user", token: secondToken, info: secondInfo, wantServer: "server-b"},
				{name: "other user token", token: f.Users[1].Token, info: f.Users[1].Info, wantServer: "server-a"},
			} {
				t.Run(routeKind+"/"+tc.name, func(t *testing.T) {
					body := map[string]any{"ItemId": item, "PlaySessionId": f.Sessions[0]}
					if routeKind == "media" {
						body["MediaSourceId"] = f.Media[1]
					}
					serverID, found := f.App.translateSessionBodyIDs(&RequestContext{
						ProxyToken: tc.token, ProxyUser: tc.info, PlaybackDeviceID: phase7KDeviceID,
					}, body)
					if routeKind == "media" && tc.wantServer == "server-a" {
						if found {
							t.Fatalf("unproven cross-source session accepted: %#v", body)
						}
						return
					}
					wantItem := "route-item-a"
					if tc.wantServer == "server-b" {
						wantItem = "route-item-b"
					}
					if !found || serverID != tc.wantServer || body["ItemId"] != wantItem || body["PlaySessionId"] != phase7KSessionID {
						t.Fatalf("token route server=%q found=%v body=%#v want=%s", serverID, found, body, tc.wantServer)
					}
					f.AssertUnchangedExcept(t, before)
				})
			}
		}
	})
}

func TestPhase7KMergedItemSharesUserProgressButNotUpstreamLease(t *testing.T) {
	for _, path := range []string{"/Sessions/Playing", "/Sessions/Playing/Progress"} {
		t.Run(path, func(t *testing.T) {
			phase7KWithFixture(t, func(f *phase7KFixture) {
				item := f.App.IDStore.GetOrCreateVirtualID("merged-a", "server-a")
				f.App.IDStore.AssociateAdditionalInstance(item, "merged-b", "server-b")
				for _, user := range f.Users {
					if err := f.App.WatchStore.RecordProgress(&WatchProgress{
						ProxyUserID: user.Info.UserID, VirtualItemID: item, ServerID: "server-a",
						OriginalItemID: "merged-a", ItemType: "Movie", PositionTicks: 111,
						RuntimeTicks: 1000, IsFavorite: true, LastPlayed: 1, UpdatedAt: 1,
					}); err != nil {
						t.Fatal(err)
					}
				}
				for _, serverID := range f.Servers {
					if result := f.App.PlaybackLimiter.Reserve(f.Users[0].Info.UserID, serverID, phase7KDeviceID, item, phase7KSessionID); !result.Allowed {
						t.Fatalf("merged item lease reserve rejected: %+v", result)
					}
				}
				f.App.playbackRoutes.Activate("token:"+f.Users[0].Token, item, "server-b", phase7KSessionID, f.Sessions[0])
				f.App.playbackRoutes.RememberMediaSource("token:"+f.Users[0].Token, f.Media[1], "server-b", phase7KSessionID, f.Sessions[0])
				f.App.playbackRoutes.RememberMediaSourceItem("token:"+f.Users[0].Token, f.Media[1], item, "server-b")
				seedPhase5WatchOwner(t, f.App, f.Users[0].Info.UserID, item, "server-b", "merged-b", phase7KDeviceID, phase7KSessionID, "shared-media", 1000)
				before := f.Snapshot(t)
				ownBefore := f.App.WatchStore.GetProgress(f.Users[0].Info.UserID, item)
				otherBefore := f.App.WatchStore.GetProgress(f.Users[1].Info.UserID, item)
				if ownBefore == nil || otherBefore == nil {
					t.Fatal("merged item progress missing")
				}
				body := f.Body(0, 1, 700)
				body["ItemId"], body["PlaySessionId"] = item, f.Sessions[0]
				rr := phase1ESessionPost(t, f.Handler, path, f.Users[0].Token, phase7KDeviceID, body)
				phase7KAssertEmptySuccess(t, rr)
				ownAfter := f.App.WatchStore.GetProgress(f.Users[0].Info.UserID, item)
				otherAfter := f.App.WatchStore.GetProgress(f.Users[1].Info.UserID, item)
				if ownAfter == nil || ownAfter.LastPlayed <= ownBefore.LastPlayed || ownAfter.UpdatedAt <= ownBefore.UpdatedAt {
					t.Fatalf("merged progress timestamps did not advance: %+v", ownAfter)
				}
				want := *ownBefore
				want.ServerID, want.OriginalItemID = "server-b", "merged-b"
				want.PositionTicks, want.RuntimeTicks = 700, 2000
				want.LastPlayed, want.UpdatedAt = ownAfter.LastPlayed, ownAfter.UpdatedAt
				if *ownAfter != want || otherAfter == nil || *otherAfter != *otherBefore {
					t.Fatalf("merged item user isolation failed own=%+v other=%+v", ownAfter, otherAfter)
				}
				f.AssertUnchangedExcept(t, before, f.Key(0, 1))
				after := f.Snapshot(t)
				for key, progress := range before.Progress {
					if after.Progress[key] != progress {
						t.Fatalf("merged event changed an unrelated virtual item: %+v", key)
					}
				}
				targetBefore, targetAfter := before.Leases[f.Key(0, 1)], after.Leases[f.Key(0, 1)]
				if targetAfter.LastHeartbeat <= targetBefore.LastHeartbeat {
					t.Fatal("merged target heartbeat did not advance")
				}
				targetBefore.LastHeartbeat = targetAfter.LastHeartbeat
				if targetBefore != targetAfter {
					t.Fatal("merged event changed target lease identity")
				}
				requests := f.Upstreams[1].Requests()
				if len(requests) != 1 || len(f.Upstreams[0].Requests()) != 0 || requests[0].Body["ItemId"] != "merged-b" {
					t.Fatalf("merged event selected wrong upstream: %+v", requests)
				}
			})
		})
	}
}
