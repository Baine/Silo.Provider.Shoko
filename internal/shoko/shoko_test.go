package shoko

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"
)

func TestNewRejectsRelativeURL(t *testing.T) {
	if _, err := New("shoko:8111", "key"); err == nil {
		t.Fatal("expected an error for a URL without a scheme")
	}
	if _, err := New("ftp://shoko", "key"); err == nil {
		t.Fatal("expected an error for a non-HTTP scheme")
	}
}

func TestCurrentUserSendsAPIKeyHeader(t *testing.T) {
	var sawKey string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		sawKey = r.Header.Get("apikey")
		if r.URL.Path != "/api/v3/User/Current" {
			t.Errorf("path = %q", r.URL.Path)
		}
		_, _ = io.WriteString(w, `{"ID":3,"Username":"silo"}`)
	}))
	t.Cleanup(server.Close)

	client, err := New(server.URL+"/", "secret-key")
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	user, err := client.CurrentUser(context.Background())
	if err != nil {
		t.Fatalf("CurrentUser: %v", err)
	}
	if sawKey != "secret-key" {
		t.Fatalf("apikey header = %q", sawKey)
	}
	if user.ID != 3 || user.Username != "silo" {
		t.Fatalf("user = %+v", user)
	}
}

func TestSetWatchedSendsSingleFieldPatch(t *testing.T) {
	var method, path, contentType string
	var body []byte
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		method, path, contentType = r.Method, r.URL.Path, r.Header.Get("Content-Type")
		body, _ = io.ReadAll(r.Body)
		w.WriteHeader(http.StatusNoContent)
	}))
	t.Cleanup(server.Close)

	client, err := New(server.URL, "secret-key")
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	watchedAt := time.Date(2026, 5, 1, 12, 30, 0, 0, time.UTC)
	if err := client.SetWatched(context.Background(), 42, &watchedAt); err != nil {
		t.Fatalf("SetWatched: %v", err)
	}
	if method != http.MethodPatch {
		t.Fatalf("method = %q", method)
	}
	if path != "/api/v3/Episode/42/UserData" {
		t.Fatalf("path = %q", path)
	}
	if contentType != "application/json-patch+json" {
		t.Fatalf("content type = %q", contentType)
	}

	var patch []map[string]any
	if err := json.Unmarshal(body, &patch); err != nil {
		t.Fatalf("decode patch: %v", err)
	}
	if len(patch) != 1 {
		t.Fatalf("patch has %d operations, want exactly one so other user data is untouched", len(patch))
	}
	if patch[0]["op"] != "add" || patch[0]["path"] != "/LastPlayedAt" {
		t.Fatalf("patch = %v", patch[0])
	}
	if patch[0]["value"] != "2026-05-01T12:30:00.000Z" {
		t.Fatalf("value = %v", patch[0]["value"])
	}
}

func TestSetWatchedClearsWithExplicitNull(t *testing.T) {
	var body []byte
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ = io.ReadAll(r.Body)
		w.WriteHeader(http.StatusNoContent)
	}))
	t.Cleanup(server.Close)

	client, err := New(server.URL, "secret-key")
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	if err := client.SetWatched(context.Background(), 42, nil); err != nil {
		t.Fatalf("SetWatched: %v", err)
	}
	var patch []map[string]any
	if err := json.Unmarshal(body, &patch); err != nil {
		t.Fatalf("decode patch: %v", err)
	}
	value, present := patch[0]["value"]
	if !present || value != nil {
		t.Fatalf("value = %v (present %v), want an explicit null", value, present)
	}
}

func TestUnauthorizedIsClassified(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusUnauthorized)
		_, _ = io.WriteString(w, `{"message":"nope"}`)
	}))
	t.Cleanup(server.Close)

	client, err := New(server.URL, "bad-key")
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	_, err = client.CurrentUser(context.Background())
	if !errors.Is(err, ErrUnauthorized) {
		t.Fatalf("error = %v, want ErrUnauthorized", err)
	}
	var apiErr *APIError
	if !errors.As(err, &apiErr) || apiErr.StatusCode != http.StatusUnauthorized {
		t.Fatalf("error does not wrap an APIError: %v", err)
	}
}

func TestWatchedEpisodesPagesUntilTotal(t *testing.T) {
	var pages int
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		pages++
		if r.URL.Query().Get("includeWatched") != "Only" {
			t.Errorf("includeWatched = %q", r.URL.Query().Get("includeWatched"))
		}
		if r.URL.Query().Get("includeDataFrom") != "AniDB,TMDB" {
			t.Errorf("includeDataFrom = %q", r.URL.Query().Get("includeDataFrom"))
		}
		page := r.URL.Query().Get("page")
		if page == "1" {
			writeList(t, w, 1001, 1000, `{"IDs":{"ID":1}}`)
			return
		}
		writeList(t, w, 1001, 1, `{"IDs":{"ID":2}}`)
	}))
	t.Cleanup(server.Close)

	client, err := New(server.URL, "secret-key")
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	episodes, err := client.WatchedEpisodes(context.Background(), true)
	if err != nil {
		t.Fatalf("WatchedEpisodes: %v", err)
	}
	if len(episodes) != 1001 {
		t.Fatalf("episodes = %d", len(episodes))
	}
	if pages != 2 {
		t.Fatalf("pages = %d", pages)
	}
}

// writeList writes a ListResult envelope with total items over count entries.
func writeList(t *testing.T, w http.ResponseWriter, total, count int, item string) {
	t.Helper()
	items := "["
	for i := 0; i < count; i++ {
		if i > 0 {
			items += ","
		}
		items += item
	}
	items += "]"
	_, _ = io.WriteString(w, `{"Total":`+itoa(total)+`,"List":`+items+`}`)
}

func itoa(value int) string {
	if value == 0 {
		return "0"
	}
	var digits []byte
	for value > 0 {
		digits = append([]byte{byte('0' + value%10)}, digits...)
		value /= 10
	}
	return string(digits)
}

func TestEpisodeTypeAcceptsNamesAndNumbers(t *testing.T) {
	var episode Episode
	if err := json.Unmarshal([]byte(`{"AniDB":{"Type":"Special","EpisodeNumber":2}}`), &episode); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if episode.AniDB.Type != EpisodeTypeSpecial || episode.AniDB.EpisodeNumber != 2 {
		t.Fatalf("episode = %+v", episode.AniDB)
	}
	if err := json.Unmarshal([]byte(`{"AniDB":{"Type":1}}`), &episode); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if episode.AniDB.Type != EpisodeTypeEpisode {
		t.Fatalf("type = %v", episode.AniDB.Type)
	}
}

func TestWatchedTimestampParsesFractionalSeconds(t *testing.T) {
	var episode Episode
	if err := json.Unmarshal([]byte(`{"Watched":"2026-05-01T12:30:00.1234567Z"}`), &episode); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if episode.Watched == nil || episode.Watched.UTC().Second() != 0 {
		t.Fatalf("watched = %v", episode.Watched)
	}
}

func TestAniDBDetailsAreAlwaysRequested(t *testing.T) {
	var seen []string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		seen = append(seen, r.URL.Query().Get("includeDataFrom"))
		writeList(t, w, 0, 0, "")
	}))
	t.Cleanup(server.Close)

	client, err := New(server.URL, "secret-key")
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	if _, err := client.WatchedEpisodes(context.Background(), false); err != nil {
		t.Fatalf("WatchedEpisodes: %v", err)
	}
	if len(seen) != 1 || seen[0] != "AniDB" {
		t.Fatalf("includeDataFrom = %v, want AniDB so episode numbers are present", seen)
	}
}

func TestSeriesUsesThePageSizeTheEndpointAccepts(t *testing.T) {
	var pageSize string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		pageSize = r.URL.Query().Get("pageSize")
		writeList(t, w, 0, 0, "")
	}))
	t.Cleanup(server.Close)

	client, err := New(server.URL, "secret-key")
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	if _, err := client.Series(context.Background()); err != nil {
		t.Fatalf("Series: %v", err)
	}
	if pageSize != "100" {
		t.Fatalf("pageSize = %q, want 100 because the Series endpoint rejects anything above it", pageSize)
	}
}

func TestSeriesRequestsAniDBType(t *testing.T) {
	var query string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		query = r.URL.RawQuery
		_, _ = io.WriteString(w, `{"Total":2,"List":[
			{"IDs":{"ID":10,"AniDB":900,"TvDB":[700],"TMDB":{"Show":[477449]}},"Name":"Film","AniDB":{"Type":0}},
			{"IDs":{"ID":11,"AniDB":901,"TvDB":[701]},"Name":"Show","AniDB":{"Type":1}}
		]}`)
	}))
	t.Cleanup(server.Close)

	client, err := New(server.URL, "secret-key")
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	series, err := client.Series(context.Background())
	if err != nil {
		t.Fatalf("Series: %v", err)
	}
	if !strings.Contains(query, "includeDataFrom=AniDB") {
		t.Fatalf("query = %q, want the AniDB side data", query)
	}
	if len(series) != 2 {
		t.Fatalf("series = %d, want 2", len(series))
	}
	if series[0].AniDB == nil || series[0].AniDB.Type != AnimeTypeMovie {
		t.Fatalf("first series type = %+v, want a movie", series[0].AniDB)
	}
	if series[1].AniDB == nil || series[1].AniDB.Type == AnimeTypeMovie {
		t.Fatalf("second series type = %+v, want not a movie", series[1].AniDB)
	}
}

func TestTimeSpanParsesShokoForms(t *testing.T) {
	cases := []struct {
		name  string
		input string
		want  time.Duration
	}{
		{"hours minutes seconds", `"00:24:00"`, 24 * time.Minute},
		{"fractional seconds", `"00:00:05.5000000"`, 5500 * time.Millisecond},
		{"days", `"1.02:03:04"`, 24*time.Hour + 2*time.Hour + 3*time.Minute + 4*time.Second},
		{"ticks", `123450000`, 12345 * time.Millisecond},
		{"null", `null`, 0},
		{"empty", `""`, 0},
	}
	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			var span TimeSpan
			if err := json.Unmarshal([]byte(testCase.input), &span); err != nil {
				t.Fatalf("unmarshal: %v", err)
			}
			if span.Duration != testCase.want {
				t.Fatalf("duration = %v, want %v", span.Duration, testCase.want)
			}
		})
	}
}

func TestScrobbleSendsPositionInTicks(t *testing.T) {
	var (
		gotPath  string
		gotQuery string
	)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		gotQuery = r.URL.RawQuery
		w.WriteHeader(http.StatusAccepted)
	}))
	t.Cleanup(server.Close)

	client, err := New(server.URL, "key")
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	position := 90 * time.Second
	if err := client.Scrobble(context.Background(), 77, ScrobblePause, nil, &position); err != nil {
		t.Fatalf("Scrobble: %v", err)
	}

	if gotPath != "/api/v3/File/77/Scrobble" {
		t.Fatalf("path = %q", gotPath)
	}
	values, err := url.ParseQuery(gotQuery)
	if err != nil {
		t.Fatalf("parse query: %v", err)
	}
	if values.Get("event") != "pause" {
		t.Fatalf("event = %q", values.Get("event"))
	}
	if values.Get("resumePosition") != "900000000" {
		t.Fatalf("resumePosition = %q, want ticks", values.Get("resumePosition"))
	}
	if _, ok := values["watched"]; ok {
		t.Fatalf("watched should be absent when it is not being changed, got %q", values.Get("watched"))
	}
}

func TestStatsReadsEnvelopeTotals(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Query().Get("pageSize") != "1" {
			t.Errorf("pageSize = %q, want 1 so the counts cost one row each", r.URL.Query().Get("pageSize"))
		}
		switch {
		case r.URL.Path == "/api/v3/Series":
			_, _ = io.WriteString(w, `{"Total":12,"List":[{}]}`)
		case r.URL.Query().Get("includeWatched") == "Only":
			_, _ = io.WriteString(w, `{"Total":40,"List":[{}]}`)
		default:
			_, _ = io.WriteString(w, `{"Total":300,"List":[{}]}`)
		}
	}))
	t.Cleanup(server.Close)

	client, err := New(server.URL, "key")
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	stats, err := client.Stats(context.Background())
	if err != nil {
		t.Fatalf("Stats: %v", err)
	}
	if stats.Series != 12 || stats.Episodes != 300 || stats.Watched != 40 {
		t.Fatalf("stats = %+v", stats)
	}
}
