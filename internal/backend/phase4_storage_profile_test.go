package backend

import (
	"fmt"
	"testing"
)

// These benchmarks deliberately use an isolated SQLite file and never touch
// production mappings. All values represent Go/SQLite on this test host.
func BenchmarkPhase4KnownItemMapping(b *testing.B) {
	store, err := NewIDStore(b.TempDir(), nil)
	if err != nil {
		b.Fatal(err)
	}
	defer store.Close()
	expected := store.GetOrCreateVirtualID("known-item", "upstream-a")
	b.ReportAllocs()
	b.ResetTimer()
	for n := 0; n < b.N; n++ {
		if got := store.GetOrCreateVirtualID("known-item", "upstream-a"); got != expected {
			b.Fatalf("unstable mapping: %s", got)
		}
	}
}
func BenchmarkPhase4NewItemMapping(b *testing.B) {
	store, err := NewIDStore(b.TempDir(), nil)
	if err != nil {
		b.Fatal(err)
	}
	defer store.Close()
	b.ReportAllocs()
	b.ResetTimer()
	for n := 0; n < b.N; n++ {
		if got := store.GetOrCreateVirtualID(fmt.Sprintf("new-item-%06d", n), "upstream-a"); got == "" {
			b.Fatal("failed to mint mapping")
		}
	}
}
func BenchmarkPhase4KnownSourceMapping(b *testing.B) {
	store, err := NewIDStore(b.TempDir(), nil)
	if err != nil {
		b.Fatal(err)
	}
	defer store.Close()
	expected, err := store.GetMergeMediaSourceID("upstream-a", "item-a", "source-a")
	if err != nil {
		b.Fatal(err)
	}
	b.ReportAllocs()
	b.ResetTimer()
	for n := 0; n < b.N; n++ {
		got, err := store.GetMergeMediaSourceID("upstream-a", "item-a", "source-a")
		if err != nil || got != expected {
			b.Fatal(err, got)
		}
	}
}
func BenchmarkPhase4NewSourceMapping(b *testing.B) {
	store, err := NewIDStore(b.TempDir(), nil)
	if err != nil {
		b.Fatal(err)
	}
	defer store.Close()
	b.ReportAllocs()
	b.ResetTimer()
	for n := 0; n < b.N; n++ {
		id, err := store.GetMergeMediaSourceID("upstream-a", "item-a", fmt.Sprintf("source-%06d", n))
		if err != nil || id == "" {
			b.Fatal(err)
		}
	}
}
func BenchmarkPhase4DeepCloneMap(b *testing.B) {
	payload := map[string]any{
		"Id": "item-a", "Type": "Movie", "Name": "Example",
		"ProviderIds": map[string]any{"Tmdb": "100", "Imdb": "tt12345"},
		"MediaSources": []any{
			map[string]any{"Id": "v1", "Container": "mkv", "Bitrate": int64(30000000),
				"MediaStreams": []any{
					map[string]any{"Index": 0, "Type": "Video", "Codec": "hevc"},
					map[string]any{"Index": 1, "Type": "Audio", "Codec": "aac"}}},
			map[string]any{"Id": "v2", "Container": "mp4", "Bitrate": int64(8000000)},
		},
		"UserData": map[string]any{"Played": false, "PlaybackPositionTicks": int64(15000)},
	}
	b.ReportAllocs()
	b.ResetTimer()
	for n := 0; n < b.N; n++ {
		data := deepCloneMap(payload)
		if data["Id"] != "item-a" {
			b.Fatal("clone changed item")
		}
	}
}
