package backend

import "testing"

// Emby clients expect an integer PlayCount. EIO must report only the
// current proxy user's local played flag, never the shared upstream count.
func TestHillsPlayCountUsesLocalUserState(t *testing.T) {
	cases := []struct {
		name string
		row  *WatchProgress
		want int64
	}{
		{name: "no local history", want: 0},
		{name: "in progress is not played", row: &WatchProgress{PositionTicks: 200, RuntimeTicks: 1000}, want: 0},
		{name: "locally marked played", row: &WatchProgress{Played: true}, want: 1},
		{name: "played and rewatching", row: &WatchProgress{Played: true, PositionTicks: 200, RuntimeTicks: 1000}, want: 1},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			ud := map[string]any{
				"PlayCount": 91, "Rating": 8, "UnplayedItemCount": 13,
				"Played": true, "PlaybackPositionTicks": int64(900),
			}
			applyUserDataState(ud, tc.row)
			got, ok := numericInt64(ud["PlayCount"])
			if !ok || got != tc.want {
				t.Fatalf("PlayCount=%v, want local count %d", ud["PlayCount"], tc.want)
			}
			for _, key := range []string{"Rating", "UnplayedItemCount"} {
				if _, found := ud[key]; found {
					t.Fatalf("%s leaked shared upstream user state", key)
				}
			}
		})
	}
}
