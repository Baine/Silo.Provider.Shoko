package plugin

import (
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	pluginv1 "github.com/Silo-Server/silo-plugin-sdk/pkg/pluginproto/silo/plugin/v1"
	publicmanifest "github.com/Silo-Server/silo-plugin-sdk/pkg/pluginsdk/manifest"
	"google.golang.org/protobuf/types/known/structpb"
	"google.golang.org/protobuf/types/known/timestamppb"

	"github.com/Baine/Silo.Provider.Shoko/internal/shoko"
)

func TestManifestIsValid(t *testing.T) {
	data, err := os.ReadFile(filepath.Join("..", "..", "manifest.json"))
	if err != nil {
		t.Fatalf("read manifest: %v", err)
	}
	manifest, err := publicmanifest.Load(data)
	if err != nil {
		t.Fatalf("load manifest: %v", err)
	}
	if err := publicmanifest.Validate(manifest); err != nil {
		t.Fatalf("validate manifest: %v", err)
	}
	if got := manifest.GetCapabilities()[0].GetType(); got != "watch_sync_provider.v1" {
		t.Fatalf("capability type = %q", got)
	}
}

// The page is a plain HTML form, so the host must route the POST the browser
// sends back to the same path. The proxy matches method and path exactly, so a
// GET-only descriptor answers 404 to the submit and the browser falls back to
// the SPA shell.
func TestConnectionTestRouteAcceptsGetAndPost(t *testing.T) {
	data, err := os.ReadFile(filepath.Join("..", "..", "manifest.json"))
	if err != nil {
		t.Fatalf("read manifest: %v", err)
	}
	manifest, err := publicmanifest.Load(data)
	if err != nil {
		t.Fatalf("load manifest: %v", err)
	}
	methods := map[string]string{}
	for _, route := range manifest.GetHttpRoutes() {
		if route.GetPath() != connectionTestPath {
			continue
		}
		if route.GetAccess() != "admin" {
			t.Fatalf("route %s access = %q, want admin", route.GetId(), route.GetAccess())
		}
		methods[route.GetMethod()] = route.GetId()
	}
	for _, method := range []string{"GET", "POST"} {
		if methods[method] == "" {
			t.Fatalf("manifest declares no %s route for %s", method, connectionTestPath)
		}
	}
	if methods["GET"] == methods["POST"] {
		t.Fatalf("GET and POST share route id %q", methods["GET"])
	}
}

// An absolute action would post to the host root, which the SPA answers with
// the start page instead of this plugin's route.
func TestConnectionTestFormPostsToItself(t *testing.T) {
	page := string(NewWeb(NewServer()).renderForm("", "", ""))
	if strings.Contains(page, `action="/`) {
		t.Fatalf("form action is absolute")
	}
	if !strings.Contains(page, `<form method="post">`) {
		t.Fatalf("form does not post to its own URL")
	}
}

func configure(t *testing.T, server *Server, fields map[string]any) {
	t.Helper()
	value, err := structpb.NewStruct(fields)
	if err != nil {
		t.Fatalf("build settings: %v", err)
	}
	if err := server.Configure(t.Context(), []*pluginv1.ConfigEntry{{Key: "shoko", Value: value}}); err != nil {
		t.Fatalf("Configure: %v", err)
	}
}

func TestConfigureStoresSettings(t *testing.T) {
	server := NewServer()
	configure(t, server, map[string]any{"base_url": "http://shoko:8111", "numbering": "tmdb"})

	set, err := server.settingsFor(" secret ")
	if err != nil {
		t.Fatalf("settingsFor: %v", err)
	}
	if set.baseURL != "http://shoko:8111" || set.apiKey != "secret" || set.numbering != numberingTMDB {
		t.Fatalf("unexpected settings: %+v", set)
	}
}

func TestConfigureAcceptsEmptyEntries(t *testing.T) {
	server := NewServer()
	if err := server.Configure(t.Context(), nil); err != nil {
		t.Fatalf("Configure with no entries = %v, want nil so the host can still start the plugin", err)
	}
	_, err := server.settingsFor("secret")
	if fault := asFault(err); fault.code != pluginv1.WatchSyncFaultCode_WATCH_SYNC_FAULT_CODE_INVALID_REQUEST {
		t.Fatalf("fault code = %v", fault.code)
	}
}

func TestConfigureRejectsNonStringField(t *testing.T) {
	server := NewServer()
	value, err := structpb.NewStruct(map[string]any{"base_url": 42})
	if err != nil {
		t.Fatal(err)
	}
	if err := server.Configure(t.Context(), []*pluginv1.ConfigEntry{{Key: "shoko", Value: value}}); err == nil {
		t.Fatal("expected an error for a non-string base_url")
	}
}

func TestSettingsForRejectsMissingAPIKey(t *testing.T) {
	server := NewServer()
	configure(t, server, map[string]any{"base_url": "http://shoko:8111"})
	_, err := server.settingsFor("")
	if fault := asFault(err); fault.code != pluginv1.WatchSyncFaultCode_WATCH_SYNC_FAULT_CODE_INVALID_CREDENTIAL {
		t.Fatalf("fault code = %v", fault.code)
	}
}

func watchedEpisode(id, aniDBEpisode, number int, episodeType shoko.EpisodeType, watchCount int) shoko.Episode {
	watched := time.Date(2026, 5, 1, 12, 0, 0, 0, time.UTC)
	return shoko.Episode{
		IDs: shoko.EpisodeIDs{
			ID:           id,
			ParentSeries: 10,
			AniDB:        aniDBEpisode,
			TvDB:         []int{500 + id},
		},
		Name:       "Episode",
		Watched:    &watched,
		WatchCount: watchCount,
		AniDB:      &shoko.AniDBEpisode{ID: aniDBEpisode, Type: episodeType, EpisodeNumber: number},
	}
}

func testSeries() map[int]shoko.Series {
	series := shoko.Series{Name: "Series"}
	series.IDs.ID = 10
	series.IDs.TvDB = []int{700}
	return map[int]shoko.Series{10: series}
}

func TestBuildStatesUsesAniDBNumbering(t *testing.T) {
	episodes := []shoko.Episode{
		watchedEpisode(1, 100, 3, shoko.EpisodeTypeEpisode, 0),
		watchedEpisode(2, 101, 4, shoko.EpisodeTypeCredits, 2),
	}
	items, warnings := buildStates(episodes, testSeries(), numberingAniDB)

	if len(items) != 1 {
		t.Fatalf("items = %d, want 1 (credits are not episodes)", len(items))
	}
	if len(warnings) != 1 {
		t.Fatalf("warnings = %v, want one note about skipped records", warnings)
	}
	media := items[0].GetMedia()
	if media.GetSeasonNumber() != 1 || media.GetEpisodeNumber() != 3 {
		t.Fatalf("season/episode = %d/%d", media.GetSeasonNumber(), media.GetEpisodeNumber())
	}
	if got := items[0].GetWatched().GetPlayCount(); got != 1 {
		t.Fatalf("play count = %d, want at least 1", got)
	}
	if got := items[0].GetProviderItemKey(); got != "shoko:episode:1" {
		t.Fatalf("provider item key = %q", got)
	}
	if got := media.GetSeriesExternalIds()["tvdb"]; got != "700" {
		t.Fatalf("series tvdb id = %q", got)
	}
}

func TestBuildStatesUsesTMDBNumbering(t *testing.T) {
	episode := watchedEpisode(1, 100, 14, shoko.EpisodeTypeEpisode, 1)
	episode.TMDB = &shoko.TmdbEpisodeData{Episodes: []shoko.TmdbEpisode{{SeasonNumber: 2, EpisodeNumber: 1}}}

	items, _ := buildStates([]shoko.Episode{episode}, testSeries(), numberingTMDB)
	if len(items) != 1 {
		t.Fatalf("items = %d", len(items))
	}
	media := items[0].GetMedia()
	if media.GetSeasonNumber() != 2 || media.GetEpisodeNumber() != 1 {
		t.Fatalf("season/episode = %d/%d", media.GetSeasonNumber(), media.GetEpisodeNumber())
	}
	if media.GetAbsoluteEpisodeNumber() != 14 {
		t.Fatalf("absolute episode = %d", media.GetAbsoluteEpisodeNumber())
	}
}

func TestBuildStatesSkipsUnwatched(t *testing.T) {
	episode := watchedEpisode(1, 100, 1, shoko.EpisodeTypeEpisode, 1)
	episode.Watched = nil
	items, _ := buildStates([]shoko.Episode{episode}, testSeries(), numberingAniDB)
	if len(items) != 0 {
		t.Fatalf("items = %d, want 0", len(items))
	}
}

func TestResolveEpisodeByProviderItemKey(t *testing.T) {
	index := &episodeIndex{byID: map[string]int{}, bySeriesEpisode: map[string]int{}}
	id, ok := resolveEpisode(index, &pluginv1.WatchSyncEvent{ProviderItemKey: "shoko:episode:42"})
	if !ok || id != 42 {
		t.Fatalf("resolved %d, %v", id, ok)
	}
}

func TestResolveEpisodeBySeriesAndNumbers(t *testing.T) {
	index := &episodeIndex{
		byID:            map[string]int{},
		bySeriesEpisode: map[string]int{"tvdb:700:2:5": 42},
	}
	event := &pluginv1.WatchSyncEvent{Media: &pluginv1.WatchSyncMedia{
		MediaType:         pluginv1.WatchSyncMediaType_WATCH_SYNC_MEDIA_TYPE_EPISODE,
		SeriesExternalIds: map[string]string{"tvdb": "700"},
		SeasonNumber:      2,
		EpisodeNumber:     5,
	}}
	id, ok := resolveEpisode(index, event)
	if !ok || id != 42 {
		t.Fatalf("resolved %d, %v", id, ok)
	}
}

func TestResolveEpisodeByEpisodeIDFallsBack(t *testing.T) {
	index := &episodeIndex{
		byID:            map[string]int{"tvdb:501": 7},
		bySeriesEpisode: map[string]int{},
	}
	event := &pluginv1.WatchSyncEvent{Media: &pluginv1.WatchSyncMedia{
		MediaType:   pluginv1.WatchSyncMediaType_WATCH_SYNC_MEDIA_TYPE_EPISODE,
		ExternalIds: map[string]string{"tvdb": "501"},
	}}
	id, ok := resolveEpisode(index, event)
	if !ok || id != 7 {
		t.Fatalf("resolved %d, %v", id, ok)
	}
}

func TestResolveEpisodeIgnoresUnknownMovie(t *testing.T) {
	index := &episodeIndex{byMovie: map[string]int{"imdb:tt0000001": 7}}
	event := &pluginv1.WatchSyncEvent{Media: &pluginv1.WatchSyncMedia{
		MediaType:   pluginv1.WatchSyncMediaType_WATCH_SYNC_MEDIA_TYPE_MOVIE,
		ExternalIds: map[string]string{"imdb": "tt9999999"},
	}}
	if _, ok := resolveEpisode(index, event); ok {
		t.Fatal("an unknown film must not resolve")
	}
}

func TestParsePageToken(t *testing.T) {
	traversal, offset, err := parsePageToken("")
	if err != nil || traversal == "" || offset != 0 {
		t.Fatalf("fresh token: %q %d %v", traversal, offset, err)
	}
	traversal, offset, err = parsePageToken("abc:200")
	if err != nil || traversal != "abc" || offset != 200 {
		t.Fatalf("continued token: %q %d %v", traversal, offset, err)
	}
	if _, _, err := parsePageToken("garbage"); err == nil {
		t.Fatal("expected an error for a foreign page token")
	}
}

func TestApplyEventRejectsUnsupportedOperation(t *testing.T) {
	server := NewServer()
	result := server.applyEvent(t.Context(), nil, nil, nil, &pluginv1.WatchSyncEvent{
		EventId:   "e1",
		Operation: pluginv1.WatchSyncOperation_WATCH_SYNC_OPERATION_ADD_FAVORITE,
	})
	if result.GetStatus() != pluginv1.WatchSyncApplyStatus_WATCH_SYNC_APPLY_STATUS_REJECTED {
		t.Fatalf("status = %v", result.GetStatus())
	}
}

func TestApplyEventRejectsUnmatchedItem(t *testing.T) {
	server := NewServer()
	index := &episodeIndex{byID: map[string]int{}, bySeriesEpisode: map[string]int{}}
	result := server.applyEvent(t.Context(), nil, index, nil, &pluginv1.WatchSyncEvent{
		EventId:   "e1",
		Operation: pluginv1.WatchSyncOperation_WATCH_SYNC_OPERATION_MARK_WATCHED,
		Media:     &pluginv1.WatchSyncMedia{MediaType: pluginv1.WatchSyncMediaType_WATCH_SYNC_MEDIA_TYPE_EPISODE},
	})
	if result.GetStatus() != pluginv1.WatchSyncApplyStatus_WATCH_SYNC_APPLY_STATUS_REJECTED {
		t.Fatalf("status = %v", result.GetStatus())
	}
	if result.GetFault().GetCode() != pluginv1.WatchSyncFaultCode_WATCH_SYNC_FAULT_CODE_PERMANENT {
		t.Fatalf("fault = %v", result.GetFault())
	}
}

func TestListRemoteStateReportsMissingConfiguration(t *testing.T) {
	server := NewServer()
	response, err := server.ListRemoteState(t.Context(), &pluginv1.WatchSyncListRemoteStateRequest{})
	if err != nil {
		t.Fatalf("unexpected transport error: %v", err)
	}
	if response.GetFault().GetCode() != pluginv1.WatchSyncFaultCode_WATCH_SYNC_FAULT_CODE_INVALID_REQUEST {
		t.Fatalf("fault = %v", response.GetFault())
	}
}

func TestLastWatchedAtIsForwarded(t *testing.T) {
	at := time.Date(2026, 5, 1, 12, 0, 0, 0, time.UTC)
	if got := timestamppb.New(at).AsTime().UTC(); !got.Equal(at) {
		t.Fatalf("round trip = %v", got)
	}
}

func movieSeries() map[int]shoko.Series {
	series := shoko.Series{
		Name:  "Film",
		AniDB: &shoko.AniDBAnime{Type: shoko.AnimeTypeMovie},
	}
	series.IDs.ID = 10
	series.IDs.IMDB = []string{"tt6634910"}
	series.IDs.TMDB.Show = []int{477449}
	return map[int]shoko.Series{10: series}
}

func TestBuildStatesDescribesAniDBMoviesAsMovies(t *testing.T) {
	episodes := []shoko.Episode{watchedEpisode(1, 100, 1, shoko.EpisodeTypeEpisode, 0)}

	items, warnings := buildStates(episodes, movieSeries(), numberingAniDB)
	if len(warnings) != 0 {
		t.Fatalf("warnings = %v, want none", warnings)
	}
	if len(items) != 1 {
		t.Fatalf("items = %d, want 1", len(items))
	}

	media := items[0].GetMedia()
	if got := media.GetMediaType(); got != pluginv1.WatchSyncMediaType_WATCH_SYNC_MEDIA_TYPE_MOVIE {
		t.Fatalf("media type = %v, want MOVIE", got)
	}
	if got := media.GetExternalIds()["imdb"]; got != "tt6634910" {
		t.Fatalf("imdb id = %q, want the film's own id", got)
	}
	if got := media.GetExternalIds()["tmdb"]; got != "477449" {
		t.Fatalf("tmdb id = %q, want the film's own id", got)
	}
	if media.GetSeasonNumber() != 0 || media.GetEpisodeNumber() != 0 {
		t.Fatalf("season/episode = %d/%d, want neither", media.GetSeasonNumber(), media.GetEpisodeNumber())
	}
}

func TestResolveEpisodeByMovieExternalID(t *testing.T) {
	index := &episodeIndex{byMovie: map[string]int{"imdb:tt6634910": 42}}
	event := &pluginv1.WatchSyncEvent{
		Media: &pluginv1.WatchSyncMedia{
			MediaType:   pluginv1.WatchSyncMediaType_WATCH_SYNC_MEDIA_TYPE_MOVIE,
			ExternalIds: map[string]string{"imdb": "tt6634910"},
		},
	}

	if id, ok := resolveEpisode(index, event); !ok || id != 42 {
		t.Fatalf("resolved id = %d, ok = %v", id, ok)
	}
}

func TestBuildStatesCollapsesOneMovieToASingleItem(t *testing.T) {
	early := watchedEpisode(1, 100, 1, shoko.EpisodeTypeEpisode, 3)
	late := watchedEpisode(2, 101, 2, shoko.EpisodeTypeEpisode, 1)
	lateDate := early.Watched.Add(48 * time.Hour)
	late.Watched = &lateDate

	items, _ := buildStates([]shoko.Episode{early, late}, movieSeries(), numberingAniDB)
	if len(items) != 1 {
		t.Fatalf("items = %d, want 1", len(items))
	}
	watched := items[0].GetWatched()
	if !watched.GetLastWatchedAt().AsTime().Equal(lateDate) {
		t.Fatalf("last watched = %v, want the later play", watched.GetLastWatchedAt().AsTime())
	}
	if got := watched.GetPlayCount(); got != 3 {
		t.Fatalf("play count = %d, want the highest count", got)
	}
}

// fakeShoko serves the handful of endpoints the write and progress paths call
// and records what the provider asked for.
type fakeShoko struct {
	*httptest.Server
	requests []*http.Request
}

func newFakeShoko(t *testing.T, handler http.HandlerFunc) *fakeShoko {
	t.Helper()
	fake := &fakeShoko{}
	fake.Server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fake.requests = append(fake.requests, r)
		w.Header().Set("Content-Type", "application/json")
		handler(w, r)
	}))
	t.Cleanup(fake.Close)
	return fake
}

func (f *fakeShoko) lastRequest(t *testing.T) *http.Request {
	t.Helper()
	if len(f.requests) == 0 {
		t.Fatal("the provider made no request")
	}
	return f.requests[len(f.requests)-1]
}

func scrobbleServer(t *testing.T, episodeJSON, seriesJSON string) *fakeShoko {
	return newFakeShoko(t, func(w http.ResponseWriter, r *http.Request) {
		switch {
		case strings.HasPrefix(r.URL.Path, "/api/v3/Episode/") && r.URL.Path != "/api/v3/Episode":
			_, _ = w.Write([]byte(episodeJSON))
		case r.URL.Path == "/api/v3/Episode":
			_, _ = w.Write([]byte(`{"Total":1,"List":[]}`))
		case r.URL.Path == "/api/v3/Series":
			_, _ = w.Write([]byte(seriesJSON))
		case strings.HasSuffix(r.URL.Path, "/Scrobble"):
			w.WriteHeader(http.StatusAccepted)
		case strings.HasSuffix(r.URL.Path, "/UserData"):
			_, _ = w.Write([]byte(`{"ProgressPosition":"00:10:00","LastUpdatedAt":"2024-05-01T20:00:00Z"}`))
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	})
}

func TestScrobbleStopAtCompletionMarksWatched(t *testing.T) {
	fake := scrobbleServer(t,
		`{"IDs":{"ID":5},"Files":[{"ID":77}]}`,
		`{"Total":0,"List":[]}`)

	server := NewServer()
	configure(t, server, map[string]any{"base_url": fake.URL})
	set, err := server.settingsFor("key")
	if err != nil {
		t.Fatalf("settingsFor: %v", err)
	}
	client, err := clientFor(set)
	if err != nil {
		t.Fatalf("clientFor: %v", err)
	}

	index := &episodeIndex{byID: map[string]int{}, bySeriesEpisode: map[string]int{}}
	result := server.applyScrobble(t.Context(), client, index, nil, &pluginv1.WatchSyncEvent{
		EventId:         "event-1",
		Operation:       pluginv1.WatchSyncOperation_WATCH_SYNC_OPERATION_SCROBBLE_STOP,
		ProviderItemKey: "shoko:episode:5",
		PositionSeconds: 1440,
		DurationSeconds: 1440,
		Completed:       true,
	})

	if got := result.GetStatus(); got != pluginv1.WatchSyncApplyStatus_WATCH_SYNC_APPLY_STATUS_APPLIED {
		t.Fatalf("status = %v, fault = %v", got, result.GetFault())
	}
	request := fake.lastRequest(t)
	if request.Method != http.MethodPatch || request.URL.Path != "/api/v3/File/77/Scrobble" {
		t.Fatalf("%s %s", request.Method, request.URL.Path)
	}
	query := request.URL.Query()
	if query.Get("watched") != "true" {
		t.Fatalf("watched = %q", query.Get("watched"))
	}
	if query.Get("resumePosition") != "" {
		t.Fatalf("resumePosition = %q, want it omitted once the play finished", query.Get("resumePosition"))
	}
}

func TestScrobblePauseSendsPosition(t *testing.T) {
	fake := scrobbleServer(t,
		`{"IDs":{"ID":5},"Files":[{"ID":77}]}`,
		`{"Total":0,"List":[]}`)

	server := NewServer()
	configure(t, server, map[string]any{"base_url": fake.URL})
	set, _ := server.settingsFor("key")
	client, _ := clientFor(set)

	index := &episodeIndex{byID: map[string]int{}, bySeriesEpisode: map[string]int{}}
	result := server.applyScrobble(t.Context(), client, index, nil, &pluginv1.WatchSyncEvent{
		EventId:         "event-2",
		Operation:       pluginv1.WatchSyncOperation_WATCH_SYNC_OPERATION_SCROBBLE_PAUSE,
		ProviderItemKey: "shoko:episode:5",
		PositionSeconds: 600,
		DurationSeconds: 1440,
	})

	if got := result.GetStatus(); got != pluginv1.WatchSyncApplyStatus_WATCH_SYNC_APPLY_STATUS_APPLIED {
		t.Fatalf("status = %v", got)
	}
	query := fake.lastRequest(t).URL.Query()
	if query.Get("event") != "pause" {
		t.Fatalf("event = %q", query.Get("event"))
	}
	if query.Get("resumePosition") != strconv.FormatInt(int64(10*time.Minute/100), 10) {
		t.Fatalf("resumePosition = %q, want the position in ticks", query.Get("resumePosition"))
	}
	if query.Get("watched") != "" {
		t.Fatalf("watched = %q, want it omitted for a pause", query.Get("watched"))
	}
}

func TestRequestedKindSeparatesProgressFromWatched(t *testing.T) {
	progressOnly := requestedKind([]pluginv1.WatchSyncRemoteStateKind{
		pluginv1.WatchSyncRemoteStateKind_WATCH_SYNC_REMOTE_STATE_KIND_PROGRESS,
	})
	if progressOnly != kindProgress {
		t.Fatalf("kind = %q", progressOnly)
	}
	both := requestedKind([]pluginv1.WatchSyncRemoteStateKind{
		pluginv1.WatchSyncRemoteStateKind_WATCH_SYNC_REMOTE_STATE_KIND_PROGRESS,
		pluginv1.WatchSyncRemoteStateKind_WATCH_SYNC_REMOTE_STATE_KIND_WATCHED,
	})
	if both != kindWatched {
		t.Fatalf("kind = %q, want the watched traversal to win", both)
	}
}

func TestBuildProgressItemsReportsPartWayOnly(t *testing.T) {
	fake := newFakeShoko(t, func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.URL.Path == "/api/v3/Episode":
			_, _ = w.Write([]byte(`{"Total":2,"List":[
				{"IDs":{"ID":5,"ParentSeries":10},"Name":"Episode","Duration":"00:24:00","ResumePosition":"00:10:00","Files":[{"ID":77}],"AniDB":{"ID":1,"Type":1,"EpisodeNumber":1}},
				{"IDs":{"ID":6,"ParentSeries":10},"Name":"Finished","Duration":"00:24:00","ResumePosition":"00:23:00","Files":[{"ID":78}],"AniDB":{"ID":2,"Type":1,"EpisodeNumber":2}}
			]}`))
		case r.URL.Path == "/api/v3/Series":
			_, _ = w.Write([]byte(`{"Total":1,"List":[{"IDs":{"ID":10,"AniDB":9,"TvDB":[700]},"Name":"Series"}]}`))
		case strings.HasSuffix(r.URL.Path, "/UserData"):
			_, _ = w.Write([]byte(`{"ProgressPosition":"00:10:00","LastUpdatedAt":"2024-05-01T20:00:00Z"}`))
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	})

	server := NewServer()
	configure(t, server, map[string]any{"base_url": fake.URL})
	set, _ := server.settingsFor("key")
	client, _ := clientFor(set)

	items, warnings, err := server.buildProgressItems(t.Context(), client, set)
	if err != nil {
		t.Fatalf("buildProgressItems: %v", err)
	}
	if len(items) != 1 {
		t.Fatalf("items = %d, want the finished play to be left to the watched traversal", len(items))
	}
	progress := items[0].GetProgress()
	if progress == nil {
		t.Fatal("item carries no progress")
	}
	if got := progress.GetProgressPercent(); got < 41 || got > 42 {
		t.Fatalf("progress = %v, want about 41.7", got)
	}
	if !progress.GetPausedAt().AsTime().Equal(time.Date(2024, 5, 1, 20, 0, 0, 0, time.UTC)) {
		t.Fatalf("paused at = %v", progress.GetPausedAt().AsTime())
	}
	if len(warnings) != 1 {
		t.Fatalf("warnings = %v, want one note about the skipped position", warnings)
	}
}
