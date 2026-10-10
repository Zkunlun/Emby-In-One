package backend

import (
	"errors"
	"fmt"
	"net/url"
	"strconv"
	"testing"
)

func TestPhase4BWindowParsingAndCountCompatibility(t *testing.T) {
	for _, invalid := range []url.Values{
		{"StartIndex": {"-1"}}, {"Limit": {"-5"}}, {"Limit": {"bad"}},
		{"StartIndex": {strconv.Itoa(int(^uint(0) >> 1))}, "Limit": {"42"}},
	} {
		if _, err := parsePassiveWindow(invalid); !errors.Is(err, errPassiveWindow) {
			t.Fatalf("accepted invalid window %v: %v", invalid, err)
		}
	}
	query := url.Values{"StartIndex": {"5001"}, "Limit": {"20"}}
	window, err := parsePassiveWindow(query)
	if err != nil || window.needed != 5021 {
		t.Fatalf("deep window %+v %v", window, err)
	}
	items := make([]map[string]any, 5021)
	for i := range items {
		items[i] = map[string]any{"Id": fmt.Sprintf("%05d", i)}
	}
	page := passiveWindowPayload(items, query, false)
	if got := len(asItems(page)); got != 20 {
		t.Fatalf("partial deep page=%d", got)
	}
	if count, _ := numericInt(page["TotalRecordCount"]); count <= 5021 {
		t.Fatalf("provisional total falsely exhausted: %d", count)
	}
	page = passiveWindowPayload(items, query, true)
	if count, _ := numericInt(page["TotalRecordCount"]); count != 5021 {
		t.Fatalf("exhausted exact count=%d", count)
	}
}

func TestPhase4BSourceCursorBeyondFormerCap(t *testing.T) {
	var source passivePageSource
	source.serverID = "A"
	pageSize := 128
	total := 6123
	pages := 0
	for !source.exhausted {
		var items []any
		for i := source.cursor; i < min(source.cursor+pageSize, total); i++ {
			items = append(items, map[string]any{"Id": fmt.Sprintf("item-%06d", i)})
		}
		payload := map[string]any{"Items": items, "TotalRecordCount": total, "StartIndex": source.cursor}
		if err := acceptPassivePage(&source, payload, pageSize); err != nil {
			t.Fatal(err)
		}
		pages++
		if pages > 100 {
			t.Fatal("cursor did not progress")
		}
	}
	if source.cursor != total || pages < 40 {
		t.Fatalf("cursor stopped early: %d / %d", source.cursor, pages)
	}
}

func TestPhase4BRejectsRepeatedPageAndInconsistentTotals(t *testing.T) {
	s := passivePageSource{serverID: "A"}
	values := []any{map[string]any{"Id": "same"}, map[string]any{"Id": "other"}}
	first := map[string]any{"Items": values, "TotalRecordCount": 8, "StartIndex": 0}
	if err := acceptPassivePage(&s, first, 2); err != nil {
		t.Fatal(err)
	}
	second := map[string]any{"Items": values, "TotalRecordCount": 8, "StartIndex": 2}
	if err := acceptPassivePage(&s, second, 2); !errors.Is(err, errPassivePage) {
		t.Fatalf("accepted repeated page %v", err)
	}
	two := passivePageSource{serverID: "B"}
	if err := acceptPassivePage(&two, first, 2); err != nil {
		t.Fatal(err)
	}
	changed := map[string]any{"Items": []any{map[string]any{"Id": "other2"}}, "TotalRecordCount": 3, "StartIndex": 2}
	if err := acceptPassivePage(&two, changed, 2); !errors.Is(err, errPassivePage) {
		t.Fatalf("accepted inconsistent total %v", err)
	}
}
