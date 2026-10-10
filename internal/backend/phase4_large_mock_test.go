package backend

import (
	"fmt"
	"net/http"
	"testing"
)

func TestPhase4EGlobalTailBeyondFiveThousand(t *testing.T) {
	if testing.Short() {
		t.Skip("large integration fixture")
	}
	items := make([]map[string]any, 5205)
	for i := range items {
		items[i] = map[string]any{"Id": fmt.Sprintf("artist-%05d", i), "Type": "MusicArtist",
			"Name": fmt.Sprintf("Artist %05d", i)}
	}
	withTask6HTTPFixture(t, items, nil, func(f *task6HTTPFixture) {
		endpoint := "/Users/" + f.user + "/Items?Recursive=true&IncludeItemTypes=MusicArtist"
		shallow := task6HTTPJSON(t, f.request(t, http.MethodGet, endpoint+"&StartIndex=0&Limit=20", nil, ""))
		if len(asItems(shallow)) != 20 {
			t.Fatalf("first window returned %d", len(asItems(shallow)))
		}
		if n := f.upstream[0].count("/Users/user-a/Items"); n != 1 {
			t.Fatalf("first window triggered %d source requests; want 1", n)
		}
		tail := task6HTTPJSON(t, f.request(t, http.MethodGet, endpoint+"&StartIndex=5010&Limit=12", nil, ""))
		if got := len(asItems(tail)); got != 12 {
			t.Fatalf("deep window beyond old cap returned %d", got)
		}
		total, ok := numericInt(tail["TotalRecordCount"])
		if !ok || total <= 5022 {
			t.Fatalf("prematurely exhausted deep total: %v", tail["TotalRecordCount"])
		}
		// An actual last page exhausts the sources and must become exact.
		end := task6HTTPJSON(t, f.request(t, http.MethodGet, endpoint+"&StartIndex=5190&Limit=15", nil, ""))
		if got := len(asItems(end)); got != 15 {
			t.Fatalf("last page length = %d, want 15", got)
		}
		if exact, valid := numericInt(end["TotalRecordCount"]); !valid || exact != 5205 {
			t.Fatalf("exhausted source must report exact count; got %v", end["TotalRecordCount"])
		}
		if n := f.upstream[0].count("/Users/user-a/Items"); n < 11 {
			t.Fatalf("deep window did not traverse source pages: %d", n)
		}
	})
}
