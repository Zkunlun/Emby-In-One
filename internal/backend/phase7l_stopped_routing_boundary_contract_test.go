package backend

import (
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"
)

func phase7LSeedMergedProgress(t *testing.T, f *phase7KFixture, item, original string) {
	t.Helper()
	for _, user := range f.Users {
		if err := f.App.WatchStore.RecordProgress(&WatchProgress{
			ProxyUserID: user.Info.UserID, VirtualItemID: item, ServerID: "server-a",
			OriginalItemID: original, ItemType: "Movie", Name: "merged fixture", IsFavorite: true,
			PositionTicks: 111, RuntimeTicks: 1000, LastPlayed: 1, UpdatedAt: 1,
		}); err != nil {
			t.Fatal(err)
		}
	}
}

func TestPhase7LStoppedMergedAliasUsesCurrentTokenAndKeepsOtherUpstreamLease(t *testing.T) {
	for _, mode := range phase7LModes() {
		for _, route := range []string{"active", "media"} {
			for tokenIndex := 0; tokenIndex < 3; tokenIndex++ {
				t.Run(mode.Name+"/"+route+"/token-"+strconv.Itoa(tokenIndex), func(t *testing.T) {
					phase7KWithFixture(t, func(f *phase7KFixture) {
						second := loginTokenAs(t, f.Handler, "phase7k-alice", "password123")
						secondInfo := f.App.Auth.ValidateToken(second)
						if secondInfo == nil || secondInfo.UserID != f.Users[0].Info.UserID || second == f.Users[0].Token {
							t.Fatal("same-user distinct token missing")
						}
						item := f.App.IDStore.GetOrCreateVirtualID("merged-a", "server-a")
						f.App.IDStore.AssociateAdditionalInstance(item, "merged-b", "server-b")
						phase7LSeedMergedProgress(t, f, item, "merged-a")
						for user := 0; user < 2; user++ {
							for server := 0; server < 2; server++ {
								if result := f.App.PlaybackLimiter.Reserve(f.Users[user].Info.UserID, f.Servers[server], phase7KDeviceID, item, phase7KSessionID); !result.Allowed {
									t.Fatal("merged lease reserve failed")
								}
							}
						}
						f.App.playbackRoutes.Activate("token:"+f.Users[0].Token, item, "server-a", phase7KSessionID, f.Sessions[0])
						f.App.playbackRoutes.Activate("token:"+second, item, "server-b", phase7KSessionID, f.Sessions[0])
						f.App.playbackRoutes.RememberMediaSource("token:"+second, f.Media[1], "server-b", phase7KSessionID, f.Sessions[0])
						f.App.IDStore.SetActiveStream(item, "server-b")
						user, server, token := 0, 0, f.Users[0].Token
						if tokenIndex == 1 {
							server, token = 1, second
						}
						if tokenIndex == 2 {
							user, token = 1, f.Users[1].Token
						}
						body := map[string]any{
							"ItemId": item, "PlaySessionId": f.Sessions[0], "UserId": f.Users[1-user].Info.UserID,
							"PositionTicks": int64(700), "RunTimeTicks": int64(2000),
						}
						if route == "media" {
							body["MediaSourceId"] = f.Media[1]
						}
						before := phase7LSnapshot(t, f)
						attempts := phase7LPrepareMode(t, f, server, mode)
						rr := phase1ESessionPost(t, f.Handler, phase7LStoppedPath, token, phase7KDeviceID, body)
						phase7LAssertResponse(t, rr, mode)
						key := phase7LWatchKey{UserID: f.Users[user].Info.UserID, ItemID: item}
						original := "merged-a"
						if server == 1 {
							original = "merged-b"
						}
						phase7LAssertProgress(t, f, before, key, f.Servers[server], original, 700, 2000, false)
						phase7LAssertLeaseAbsent(t, f, f.Key(user, server))
						phase7LAssertUnchangedExcept(t, f, before, []phase7LWatchKey{key}, []streamKey{f.Key(user, server)})
						requests := f.Upstreams[server].Requests()
						wantHits := 0
						if mode.Status != 0 {
							wantHits = 1
						}
						if len(requests) != wantHits || len(f.Upstreams[1-server].Requests()) != 0 {
							t.Fatal("merged Stopped reached wrong upstream")
						}
						wantAttempts := int32(0)
						if mode.Transport != nil {
							wantAttempts = 1
						}
						if attempts.Load() != wantAttempts {
							t.Fatalf("merged transport attempts=%d want=%d", attempts.Load(), wantAttempts)
						}
						if wantHits != 0 {
							sent := requests[0]
							position, _ := numericInt64(sent.Body["PositionTicks"])
							if sent.Path != phase7LStoppedPath || sent.Body["ItemId"] != original || sent.Body["PlaySessionId"] != phase7KSessionID || sent.Body["UserId"] != "upstream-user-"+f.Servers[server] || position != 700 {
								t.Fatalf("merged Stopped identity mismatch: %+v", sent)
							}
						}
					})
				})
			}
		}
	}
}

func TestPhase7LStoppedAdminNeverCreatesOrChangesRegularUserLocalState(t *testing.T) {
	for modeIndex, mode := range phase7LModes() {
		t.Run(mode.Name, func(t *testing.T) {
			phase7KWithFixture(t, func(f *phase7KFixture) {
				server := modeIndex % 2
				before := phase7LSnapshot(t, f)
				attempts := phase7LPrepareMode(t, f, server, mode)
				rr := phase1ESessionPost(t, f.Handler, phase7LStoppedPath, f.AdminToken, phase7KDeviceID, f.Body(0, server, 700))
				phase7LAssertResponse(t, rr, mode)
				phase7LAssertUnchangedExcept(t, f, before, nil, nil)
				phase7LAssertCalls(t, f, server, mode, attempts, phase7KSessionID, 700)
			})
		})
	}
}

func TestPhase7LStoppedForbiddenPrecedesEveryUpstreamOutcome(t *testing.T) {
	for _, mode := range phase7LModes() {
		t.Run(mode.Name, func(t *testing.T) {
			phase7KWithFixture(t, func(f *phase7KFixture) {
				limited := phase7KCreateUser(t, f, "phase7l-limited", []string{"server-a"})
				before := phase7LSnapshot(t, f)
				attempts := phase7LPrepareMode(t, f, 1, mode)
				rr := phase1ESessionPost(t, f.Handler, phase7LStoppedPath, limited.Token, phase7KDeviceID, f.Body(0, 1, 700))
				if rr.Code != http.StatusForbidden {
					t.Fatalf("forbidden Stopped=%d %s", rr.Code, rr.Body.String())
				}
				phase7LAssertUnchangedExcept(t, f, before, nil, nil)
				if attempts.Load() != 0 || len(f.Upstreams[0].Requests()) != 0 || len(f.Upstreams[1].Requests()) != 0 {
					t.Fatal("forbidden Stopped attempted an upstream request")
				}
			})
		})
	}
}

func TestPhase7LStoppedWithoutItemSeparatesProgressFromLeaseFinalization(t *testing.T) {
	for caseIndex, tc := range []struct {
		name                        string
		media, omitSession, release bool
	}{
		{name: "explicit session without item", release: true},
		{name: "media and session without item", media: true, release: true},
		{name: "media without item or session", media: true, omitSession: true},
	} {
		for _, mode := range []phase7LMode{{Name: "HTTP 204", Status: http.StatusNoContent}, {Name: "offline"}, {Name: "auth preparation"}} {
			t.Run(tc.name+"/"+mode.Name, func(t *testing.T) {
				phase7KWithFixture(t, func(f *phase7KFixture) {
					server := caseIndex % 2
					body := map[string]any{"UserId": f.Users[1].Info.UserID, "PositionTicks": int64(700), "RunTimeTicks": int64(2000)}
					if !tc.omitSession {
						body["PlaySessionId"] = f.Sessions[server]
					}
					if tc.media {
						body["MediaSourceId"] = f.Media[server]
					}
					before := phase7LSnapshot(t, f)
					attempts := phase7LPrepareMode(t, f, server, mode)
					rr := phase1ESessionPost(t, f.Handler, phase7LStoppedPath, f.Users[0].Token, phase7KDeviceID, body)
					phase7LAssertResponse(t, rr, mode)
					var changed []streamKey
					if tc.release {
						changed = []streamKey{f.Key(0, server)}
						phase7LAssertLeaseAbsent(t, f, changed[0])
					}
					phase7LAssertUnchangedExcept(t, f, before, nil, changed)
					wantHits := 0
					if mode.Status != 0 {
						wantHits = 1
					}
					requests := f.Upstreams[server].Requests()
					if len(requests) != wantHits || len(f.Upstreams[1-server].Requests()) != 0 || attempts.Load() != 0 {
						t.Fatal("itemless Stopped reached wrong upstream")
					}
					if wantHits != 0 {
						sent := requests[0]
						if _, exists := sent.Body["ItemId"]; exists {
							t.Fatal("itemless Stopped manufactured ItemId")
						}
						if sent.Path != phase7LStoppedPath || sent.Body["UserId"] != "upstream-user-"+f.Servers[server] {
							t.Fatalf("itemless Stopped identity mismatch: %+v", sent)
						}
						if tc.omitSession {
							if _, exists := sent.Body["PlaySessionId"]; exists {
								t.Fatal("itemless Stopped manufactured PlaySessionId")
							}
						} else if sent.Body["PlaySessionId"] != phase7KSessionID {
							t.Fatalf("itemless Stopped session=%v", sent.Body["PlaySessionId"])
						}
					}
				})
			})
		}
	}
}

func TestPhase7LStoppedAuthenticationAndPayloadGuardsHaveNoLocalSideEffects(t *testing.T) {
	for _, tc := range []struct {
		name, tokenKind, raw string
		status               int
	}{
		{name: "missing token", tokenKind: "missing", raw: "{}", status: http.StatusUnauthorized},
		{name: "invalid token", tokenKind: "invalid", raw: "{}", status: http.StatusUnauthorized},
		{name: "malformed JSON", raw: "{", status: http.StatusNoContent},
		{name: "array payload", raw: "[]", status: http.StatusNoContent},
		{name: "scalar payload", raw: "7", status: http.StatusNoContent},
		{name: "string payload", raw: `"value"`, status: http.StatusNoContent},
		{name: "null payload", raw: "null", status: http.StatusNoContent},
		{name: "empty body", raw: "", status: http.StatusNoContent},
		{name: "empty object", raw: "{}", status: http.StatusNoContent},
		{name: "unresolved IDs", raw: `{"ItemId":"unknown-item","PlaySessionId":"unknown-session","PositionTicks":700}`, status: http.StatusNoContent},
	} {
		t.Run(tc.name, func(t *testing.T) {
			phase7KWithFixture(t, func(f *phase7KFixture) {
				before := phase7LSnapshot(t, f)
				token := f.Users[0].Token
				if tc.tokenKind == "missing" {
					token = ""
				}
				if tc.tokenKind == "invalid" {
					token = "phase7l-invalid-token"
				}
				req := httptest.NewRequest(http.MethodPost, phase7LStoppedPath, strings.NewReader(tc.raw))
				req.Header.Set("Content-Type", "application/json")
				req.Header.Set("X-Emby-Token", token)
				req.Header.Set("X-Emby-Device-Id", phase7KDeviceID)
				rr := httptest.NewRecorder()
				f.Handler.ServeHTTP(rr, req)
				if rr.Code != tc.status {
					t.Fatalf("guard response=%d want=%d body=%s", rr.Code, tc.status, rr.Body.String())
				}
				if tc.status == http.StatusNoContent && rr.Body.Len() != 0 {
					t.Fatal("no-content guard returned a body")
				}
				phase7LAssertUnchangedExcept(t, f, before, nil, nil)
				if len(f.Upstreams[0].Requests()) != 0 || len(f.Upstreams[1].Requests()) != 0 {
					t.Fatal("guarded Stopped reached upstream")
				}
			})
		})
	}
}
