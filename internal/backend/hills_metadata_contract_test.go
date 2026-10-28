package backend

import (
	"testing"
)

// A merged work may choose its preferred Overview from one authorized upstream
// while another authorized instance is the only one with a genuine year.
// Picking one DTO must not silently erase independent portable metadata.
func TestHillsMergedMetadataRetainsKnownYearWithoutMixingParentCounts(t *testing.T) {
	a := task6HTTPMovie("movie-a", task6HTTPSource("source-a", 1000))
	b := task6HTTPMovie("movie-b", task6HTTPSource("source-b", 1000))
	a["Overview"] = "中文介绍"
	delete(a, "ProductionYear")
	b["Overview"] = "Longer English description"
	b["ChildCount"] = 17
	b["IndexNumber"] = 8
	b["ParentId"] = "unrelated-parent"
	withTask6HTTPFixture(t, []map[string]any{a}, []map[string]any{b}, func(f *task6HTTPFixture) {
		rows := f.list(t, "Movie")
		if len(rows) != 1 {
			t.Fatalf("expected one merged work, got %d", len(rows))
		}
		item := rows[0]
		if got := item["Overview"]; got != "中文介绍" {
			t.Fatalf("preferred Overview changed: %v", got)
		}
		year, ok := numericInt(item["ProductionYear"])
		if !ok || year != 2024 {
			t.Fatalf("genuine ProductionYear lost during merge: %v", item["ProductionYear"])
		}
		for _, key := range []string{"ChildCount", "IndexNumber", "ParentId"} {
			if _, has := item[key]; has {
				t.Fatalf("unrelated upstream-specific %s leaked: %v", key, item[key])
			}
		}
		sources, ok := item["MediaSources"].([]any)
		if !ok || len(sources) != 2 {
			t.Fatalf("merged media versions changed: %#v", item["MediaSources"])
		}
	})
}
