package backend

import (
	"errors"
	"io"
	"net/http"
	"strings"
	"testing"
	"time"
)

func TestMediaCountsDecodeContract(t *testing.T) {
	cases := []struct {
		name, body string
		want       mediaCounts
		valid      bool
	}{
		{"complete", `{"MovieCount":30,"SeriesCount":5,"EpisodeCount":120}`, mediaCounts{30, 5, 120}, true},
		{"zero", `{"MovieCount":0,"SeriesCount":0,"EpisodeCount":0}`, mediaCounts{}, true},
		{"official extras", `{"MovieCount":2147483647,"SeriesCount":2,"EpisodeCount":3,"SongCount":999,"extra":{"a":1}}`, mediaCounts{2147483647, 2, 3}, true},
		{"missing", `{"MovieCount":1,"SeriesCount":2}`, mediaCounts{}, false},
		{"duplicate", `{"MovieCount":1,"MovieCount":1,"SeriesCount":2,"EpisodeCount":3}`, mediaCounts{}, false},
		{"wrong case", `{"movieCount":1,"SeriesCount":2,"EpisodeCount":3}`, mediaCounts{}, false},
		{"negative", `{"MovieCount":-1,"SeriesCount":2,"EpisodeCount":3}`, mediaCounts{}, false},
		{"decimal", `{"MovieCount":1.0,"SeriesCount":2,"EpisodeCount":3}`, mediaCounts{}, false},
		{"exponent", `{"MovieCount":1e2,"SeriesCount":2,"EpisodeCount":3}`, mediaCounts{}, false},
		{"string", `{"MovieCount":"1","SeriesCount":2,"EpisodeCount":3}`, mediaCounts{}, false},
		{"null", `{"MovieCount":null,"SeriesCount":2,"EpisodeCount":3}`, mediaCounts{}, false},
		{"boolean", `{"MovieCount":true,"SeriesCount":2,"EpisodeCount":3}`, mediaCounts{}, false},
		{"source overflow", `{"MovieCount":2147483648,"SeriesCount":2,"EpisodeCount":3}`, mediaCounts{}, false},
		{"integer overflow", `{"MovieCount":99999999999999999999999,"SeriesCount":2,"EpisodeCount":3}`, mediaCounts{}, false},
		{"array", `[{"MovieCount":1,"SeriesCount":2,"EpisodeCount":3}]`, mediaCounts{}, false},
		{"html", "<html>blocked</html>", mediaCounts{}, false},
		{"empty", "", mediaCounts{}, false},
		{"trailing", `{"MovieCount":1,"SeriesCount":2,"EpisodeCount":3} {}`, mediaCounts{}, false},
		{"leading zero", `{"MovieCount":01,"SeriesCount":2,"EpisodeCount":3}`, mediaCounts{}, false},
		{"oversize", strings.Repeat(" ", countsPayloadLimit+1), mediaCounts{}, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, err := decodeMediaCounts([]byte(tc.body))
			if (err == nil) != tc.valid || (tc.valid && got != tc.want) {
				t.Fatalf("got=%+v error=%v", got, err)
			}
		})
	}
}

func TestMediaCountsBodyAndProbeContract(t *testing.T) {
	body, err := readCountsBody(strings.NewReader("1234"), 4)
	if err != nil || string(body) != "1234" {
		t.Fatal("exact limit rejected")
	}
	if _, err := readCountsBody(strings.NewReader("12345"), 4); err == nil {
		t.Fatal("oversize accepted")
	}
	if _, err := readCountsBody(nil, 4); err == nil {
		t.Fatal("nil accepted")
	}
	if _, err := readCountsBody(io.MultiReader(strings.NewReader("1"), countsTestErrorReader{}), 4); err == nil {
		t.Fatal("read error hidden")
	}
	for _, body := range []string{`{"Id":"server","Name":"ok"}`, ` {"Id":"server"} `} {
		if !validCountsProbe([]byte(body)) {
			t.Fatal("valid probe rejected")
		}
	}
	for _, body := range []string{`{}`, `{"Id":""}`, `{"Id":"  "}`, `{"Id":3}`, `{"Id":"a","Id":"b"}`, `{"Id":"a"} {}`, `[]`, "<html>", strings.Repeat(" ", countsProbeLimit+1)} {
		if validCountsProbe([]byte(body)) {
			t.Fatal("invalid probe accepted")
		}
	}
}

type countsTestErrorReader struct{}

func (countsTestErrorReader) Read([]byte) (int, error) {
	return 0, errors.New("synthetic read failure")
}

func TestMediaCountsSafeAddition(t *testing.T) {
	got, err := (mediaCounts{1, 2, 3}).add(mediaCounts{4, 5, 6})
	if err != nil || got != (mediaCounts{5, 7, 9}) {
		t.Fatal(got, err)
	}
	for _, pair := range [][2]mediaCounts{
		{{countsJSONSafeMax, 0, 0}, {1, 0, 0}}, {{0, countsJSONSafeMax, 0}, {0, 1, 0}},
		{{0, 0, countsJSONSafeMax}, {0, 0, 1}}, {{-1, 0, 0}, {}}, {{}, {0, -1, 0}},
	} {
		if _, err := pair[0].add(pair[1]); !errors.Is(err, errCountsOutOfRange) {
			t.Fatal("unsafe sum accepted")
		}
	}
	got, err = (mediaCounts{countsJSONSafeMax - 1, 0, 0}).add(mediaCounts{1, 0, 0})
	if err != nil || got.MovieCount != countsJSONSafeMax {
		t.Fatal("safe boundary rejected")
	}
}

func TestMediaCountsRetryAfterContract(t *testing.T) {
	now := time.Date(2026, 10, 5, 0, 0, 0, 0, time.UTC)
	cases := []struct {
		name                      string
		status                    int
		values                    []string
		date                      string
		limited, server, fallback bool
		delay                     time.Duration
	}{
		{"seconds", 503, []string{"120"}, "", true, true, false, 2 * time.Minute},
		{"zero minimum", 429, []string{"0"}, "", true, true, false, time.Minute},
		{"multiple longest", 503, []string{"60", "7200"}, "", true, true, false, 2 * time.Hour},
		{"missing 429", 429, nil, "", true, false, true, 0},
		{"invalid 429", 429, []string{"junk", "-1"}, "", true, false, true, 0},
		{"invalid ordinary", 500, []string{"junk"}, "", false, false, false, 0},
		{"http date", 503, []string{now.Add(3 * time.Minute).Format(http.TimeFormat)}, "", true, true, false, 3 * time.Minute},
		{"date skew", 503, []string{now.Add(time.Minute).Format(http.TimeFormat)}, now.Add(-4 * time.Minute).Format(http.TimeFormat), true, true, false, 5 * time.Minute},
		{"expired", 429, []string{now.Add(-time.Hour).Format(http.TimeFormat)}, "", true, true, false, time.Minute},
		{"huge", 429, []string{strings.Repeat("9", 100)}, "", true, true, false, countsMaximumDuration},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			headers := http.Header{"Retry-After": tc.values}
			if tc.date != "" {
				headers.Set("Date", tc.date)
			}
			got := countsRetryAfter(headers, tc.status, now)
			if got.Limited != tc.limited || got.HasServerWait != tc.server || got.NeedsFallback != tc.fallback {
				t.Fatalf("%+v", got)
			}
			if tc.server && !got.Until.Equal(now.Add(tc.delay)) {
				t.Fatalf("until=%v", got.Until)
			}
		})
	}
	for _, tc := range []struct {
		ms        int
		max, want time.Duration
	}{{0, countsAPITimeout, countsAPITimeout}, {-1, countsCheckTimeout, countsCheckTimeout}, {250, countsAPITimeout, 250 * time.Millisecond}, {60000, countsAPITimeout, countsAPITimeout}} {
		if countsOperationTimeout(tc.ms, tc.max) != tc.want {
			t.Fatal("timeout bound")
		}
	}
}
