// Package plugin implements the Silo watch_sync_provider.v1 capability on top
// of the Shoko Server API.
//
// The provider treats Shoko as the source of truth for episode watch state. It
// imports the episodes Shoko records as watched and writes Silo's watched and
// unwatched actions back into the same episode user record.
package plugin

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	pluginv1 "github.com/Silo-Server/silo-plugin-sdk/pkg/pluginproto/silo/plugin/v1"
	"google.golang.org/protobuf/types/known/structpb"
	"google.golang.org/protobuf/types/known/timestamppb"

	"github.com/Baine/Silo.Provider.Shoko/internal/shoko"
)

const (
	// configKey is the ConfigSchema key declared in manifest.json. Global
	// settings arrive as one ConfigEntry under this key whose value holds the
	// schema's properties.
	configKey = "shoko"

	fieldBaseURL   = "base_url"
	fieldNumbering = "numbering"

	numberingAniDB = "anidb"
	numberingTMDB  = "tmdb"

	providerItemKeyPrefix = "shoko:episode:"

	defaultPageSize = 100
	maxPageSize     = 1000

	snapshotTTL  = 15 * time.Minute
	maxSnapshots = 4
	indexTTL     = 5 * time.Minute
	maxIndexes   = 2

	// maxProgressTimestampLookups bounds the per-episode file lookups a progress
	// traversal makes, because each one is a request of its own.
	maxProgressTimestampLookups = 200

	// scrobbleCompletionThreshold is the share of an episode after which Shoko
	// itself treats a play as finished and clears the resume position.
	scrobbleCompletionThreshold = 95

	kindWatched  = "watched"
	kindProgress = "progress"
)

// faultError carries the provider-facing fault a failure maps to.
type faultError struct {
	code    pluginv1.WatchSyncFaultCode
	message string
}

func (e *faultError) Error() string { return e.message }

func newFault(code pluginv1.WatchSyncFaultCode, message string) error {
	return &faultError{code: code, message: message}
}

func invalidRequest(message string) error {
	return newFault(pluginv1.WatchSyncFaultCode_WATCH_SYNC_FAULT_CODE_INVALID_REQUEST, message)
}

func invalidCredential(message string) error {
	return newFault(pluginv1.WatchSyncFaultCode_WATCH_SYNC_FAULT_CODE_INVALID_CREDENTIAL, message)
}

// asFault maps an arbitrary error to a fault, keeping the classes the host
// treats differently.
func asFault(err error) *faultError {
	var fault *faultError
	if errors.As(err, &fault) {
		return fault
	}
	switch {
	case errors.Is(err, shoko.ErrUnauthorized):
		return &faultError{pluginv1.WatchSyncFaultCode_WATCH_SYNC_FAULT_CODE_INVALID_CREDENTIAL, "Shoko rejected the API key"}
	case errors.Is(err, shoko.ErrRateLimited):
		return &faultError{pluginv1.WatchSyncFaultCode_WATCH_SYNC_FAULT_CODE_RATE_LIMITED, "Shoko rate limited the request"}
	case errors.Is(err, shoko.ErrNotFound):
		return &faultError{pluginv1.WatchSyncFaultCode_WATCH_SYNC_FAULT_CODE_PERMANENT, "Shoko has no matching record"}
	}
	return &faultError{pluginv1.WatchSyncFaultCode_WATCH_SYNC_FAULT_CODE_TEMPORARY, "Shoko did not answer the request; it will be retried"}
}

func faultMessage(err error) *pluginv1.WatchSyncFault {
	fault := asFault(err)
	return &pluginv1.WatchSyncFault{Code: fault.code, SafeMessage: fault.message}
}

// settings is the resolved plugin configuration for one connection.
type settings struct {
	baseURL   string
	apiKey    string
	numbering string
}

// globalConfig is the operator-owned part of the configuration: the same Shoko
// server and numbering for every connection on this installation. It arrives
// through Runtime.Configure, not through the connection form, because an API
// key identifies a Shoko user while the server it talks to does not vary per
// user.
type globalConfig struct {
	baseURL   string
	numbering string
}

// Configure stores the settings the host saved on the plugin page. It must
// accept an empty entry list: a fresh install sends none, and the host still
// has to start the plugin to report what is missing.
func (s *Server) Configure(_ context.Context, entries []*pluginv1.ConfigEntry) error {
	conf := globalConfig{numbering: numberingAniDB}
	for _, entry := range entries {
		if entry.GetKey() != configKey {
			continue
		}
		fields := entry.GetValue().GetFields()
		if value, ok := fields[fieldBaseURL]; ok {
			text, ok := value.GetKind().(*structpb.Value_StringValue)
			if !ok {
				return fmt.Errorf("%s.%s: expected a string, got %T", configKey, fieldBaseURL, value.GetKind())
			}
			conf.baseURL = strings.TrimSpace(text.StringValue)
		}
		if value, ok := fields[fieldNumbering]; ok {
			text, ok := value.GetKind().(*structpb.Value_StringValue)
			if !ok {
				return fmt.Errorf("%s.%s: expected a string, got %T", configKey, fieldNumbering, value.GetKind())
			}
			if strings.EqualFold(strings.TrimSpace(text.StringValue), numberingTMDB) {
				conf.numbering = numberingTMDB
			}
		}
	}

	s.confMu.Lock()
	s.conf = conf
	s.confMu.Unlock()

	// A saved setting can point at another server, so cached traversals and
	// episode indexes belong to the old one.
	s.mu.Lock()
	s.snapshots = map[string]*snapshot{}
	s.indexes = map[string]*episodeIndex{}
	s.mu.Unlock()
	return nil
}

func (s *Server) configuration() globalConfig {
	s.confMu.RLock()
	defer s.confMu.RUnlock()
	return s.conf
}

// settingsFor combines the operator's settings with the connection's API key.
func (s *Server) settingsFor(apiKey string) (settings, error) {
	conf := s.configuration()
	if conf.baseURL == "" {
		return settings{}, invalidRequest("the Shoko server URL is not configured in the plugin settings")
	}
	apiKey = strings.TrimSpace(apiKey)
	if apiKey == "" {
		return settings{}, invalidCredential("the API key is missing")
	}
	return settings{baseURL: conf.baseURL, apiKey: apiKey, numbering: conf.numbering}, nil
}

func (s *Server) settingsFromContext(authenticated *pluginv1.WatchSyncAuthenticatedContext) (settings, error) {
	if authenticated == nil {
		return settings{}, invalidRequest("the provider context is missing")
	}
	return s.settingsFor(authenticated.GetCredentials().GetAccessToken())
}

func clientFor(set settings) (*shoko.Client, error) {
	client, err := shoko.New(set.baseURL, set.apiKey)
	if err != nil {
		return nil, invalidRequest(err.Error())
	}
	return client, nil
}

// account converts a Shoko user into the account the connection shows.
func account(user *shoko.User) *pluginv1.WatchSyncAccount {
	if user == nil {
		return nil
	}
	return &pluginv1.WatchSyncAccount{
		ExternalSubject: strconv.Itoa(user.ID),
		Username:        user.Username,
		DisplayName:     user.Username,
	}
}

// credentials returns the stored API key as the connection's credential. Shoko
// API keys do not expire, so nothing needs refreshing.
func credentials(set settings) *pluginv1.WatchSyncCredentials {
	return &pluginv1.WatchSyncCredentials{
		AccessToken: set.apiKey,
		TokenType:   "apikey",
	}
}

// snapshot is one cached ListRemoteState traversal.
type snapshot struct {
	items   []*pluginv1.WatchSyncRemoteState
	expires time.Time
}

// episodeIndex resolves Silo items to Shoko episodes for ApplyEvents.
type episodeIndex struct {
	byMovie map[string]int
	built   time.Time
	// byID maps "<namespace>:<value>" to a Shoko episode id.
	byID map[string]int
	// bySeriesEpisode maps "<namespace>:<series value>:<season>:<episode>" to a
	// Shoko episode id, for items that carry series ids but no episode ids.
	bySeriesEpisode map[string]int
}

// Server implements pluginv1.WatchSyncProviderServer.
type Server struct {
	pluginv1.UnimplementedWatchSyncProviderServer

	confMu sync.RWMutex
	conf   globalConfig

	mu        sync.Mutex
	snapshots map[string]*snapshot
	indexes   map[string]*episodeIndex
	files     map[int]fileRef
}

// fileRef caches which Shoko file an episode plays from, because resume
// position and playback progress are stored against the file rather than the
// episode, and resolving it costs one request per episode.
type fileRef struct {
	fileID  int
	expires time.Time
}

// NewServer returns a ready provider.
func NewServer() *Server {
	return &Server{
		conf:      globalConfig{numbering: numberingAniDB},
		snapshots: map[string]*snapshot{},
		indexes:   map[string]*episodeIndex{},
		files:     map[int]fileRef{},
	}
}

// ---------------------------------------------------------------------------
// Credentials and account
// ---------------------------------------------------------------------------

// ExchangeAPIKey validates the operator's API key against Shoko and stores it.
func (s *Server) ExchangeAPIKey(ctx context.Context, req *pluginv1.WatchSyncExchangeAPIKeyRequest) (*pluginv1.WatchSyncCredentialResponse, error) {
	set, err := s.settingsFor(req.GetApiKey())
	if err != nil {
		return &pluginv1.WatchSyncCredentialResponse{Fault: faultMessage(err)}, nil
	}
	client, err := clientFor(set)
	if err != nil {
		return &pluginv1.WatchSyncCredentialResponse{Fault: faultMessage(err)}, nil
	}
	user, err := client.CurrentUser(ctx)
	if err != nil {
		return &pluginv1.WatchSyncCredentialResponse{Fault: faultMessage(err)}, nil
	}
	return &pluginv1.WatchSyncCredentialResponse{
		Credentials: credentials(set),
		Account:     account(user),
	}, nil
}

// RefreshCredentials re-checks the stored key, which Shoko never expires.
func (s *Server) RefreshCredentials(ctx context.Context, req *pluginv1.WatchSyncRefreshCredentialsRequest) (*pluginv1.WatchSyncCredentialResponse, error) {
	set, err := s.settingsFromContext(req.GetContext())
	if err != nil {
		return &pluginv1.WatchSyncCredentialResponse{Fault: faultMessage(err)}, nil
	}
	client, err := clientFor(set)
	if err != nil {
		return &pluginv1.WatchSyncCredentialResponse{Fault: faultMessage(err)}, nil
	}
	user, err := client.CurrentUser(ctx)
	if err != nil {
		return &pluginv1.WatchSyncCredentialResponse{Fault: faultMessage(err)}, nil
	}
	return &pluginv1.WatchSyncCredentialResponse{
		Credentials: credentials(set),
		Account:     account(user),
	}, nil
}

// GetAccount returns the Shoko user the connection acts as.
func (s *Server) GetAccount(ctx context.Context, req *pluginv1.WatchSyncGetAccountRequest) (*pluginv1.WatchSyncGetAccountResponse, error) {
	set, err := s.settingsFromContext(req.GetContext())
	if err != nil {
		return &pluginv1.WatchSyncGetAccountResponse{Fault: faultMessage(err)}, nil
	}
	client, err := clientFor(set)
	if err != nil {
		return &pluginv1.WatchSyncGetAccountResponse{Fault: faultMessage(err)}, nil
	}
	user, err := client.CurrentUser(ctx)
	if err != nil {
		return &pluginv1.WatchSyncGetAccountResponse{Fault: faultMessage(err)}, nil
	}
	return &pluginv1.WatchSyncGetAccountResponse{Account: account(user)}, nil
}

// InitAuthorize is unused: the descriptor advertises API keys only.
func (s *Server) InitAuthorize(context.Context, *pluginv1.WatchSyncInitAuthorizeRequest) (*pluginv1.WatchSyncInitAuthorizeResponse, error) {
	return &pluginv1.WatchSyncInitAuthorizeResponse{
		Fault: faultMessage(invalidRequest("this provider authenticates with an API key only")),
	}, nil
}

// ExchangeCode is unused: the descriptor advertises API keys only.
func (s *Server) ExchangeCode(context.Context, *pluginv1.WatchSyncExchangeCodeRequest) (*pluginv1.WatchSyncCredentialResponse, error) {
	return &pluginv1.WatchSyncCredentialResponse{
		Fault: faultMessage(invalidRequest("this provider authenticates with an API key only")),
	}, nil
}

// ---------------------------------------------------------------------------
// Import
// ---------------------------------------------------------------------------

// ListRemoteState pages through the episodes Shoko records as watched.
func (s *Server) ListRemoteState(ctx context.Context, req *pluginv1.WatchSyncListRemoteStateRequest) (*pluginv1.WatchSyncListRemoteStateResponse, error) {
	set, err := s.settingsFromContext(req.GetContext())
	if err != nil {
		return &pluginv1.WatchSyncListRemoteStateResponse{Fault: faultMessage(err)}, nil
	}
	client, err := clientFor(set)
	if err != nil {
		return &pluginv1.WatchSyncListRemoteStateResponse{Fault: faultMessage(err)}, nil
	}

	pageSize := int(req.GetPageSize())
	if pageSize <= 0 {
		pageSize = defaultPageSize
	}
	if pageSize > maxPageSize {
		pageSize = maxPageSize
	}

	traversal, offset, err := parsePageToken(req.GetPageToken())
	if err != nil {
		return &pluginv1.WatchSyncListRemoteStateResponse{Fault: faultMessage(err)}, nil
	}

	kind := requestedKind(req.GetStateKinds())
	items, warnings, err := s.itemsForTraversal(ctx, client, set, traversal, kind)
	if err != nil {
		return &pluginv1.WatchSyncListRemoteStateResponse{Fault: faultMessage(err)}, nil
	}

	if offset > len(items) {
		offset = len(items)
	}
	end := offset + pageSize
	if end > len(items) {
		end = len(items)
	}

	response := &pluginv1.WatchSyncListRemoteStateResponse{
		Items:            items[offset:end],
		CompleteSnapshot: true,
		Warnings:         warnings,
	}
	if end < len(items) {
		response.NextPageToken = traversal + ":" + strconv.Itoa(end)
	} else {
		response.NextCursor = traversal
	}
	return response, nil
}

// parsePageToken splits "<traversal>:<offset>", minting a traversal on the
// first page.
func parsePageToken(token string) (string, int, error) {
	if token == "" {
		traversal, err := randomID()
		if err != nil {
			return "", 0, err
		}
		return traversal, 0, nil
	}
	separator := strings.LastIndex(token, ":")
	if separator <= 0 {
		return "", 0, invalidRequest("the page token is not one this provider issued")
	}
	offset, err := strconv.Atoi(token[separator+1:])
	if err != nil || offset < 0 {
		return "", 0, invalidRequest("the page token is not one this provider issued")
	}
	return token[:separator], offset, nil
}

func randomID() (string, error) {
	var buf [8]byte
	if _, err := rand.Read(buf[:]); err != nil {
		return "", fmt.Errorf("generate traversal id: %w", err)
	}
	return hex.EncodeToString(buf[:]), nil
}

// itemsForTraversal returns the cached snapshot for a traversal, fetching it on
// the first page. Caching keeps every page of one traversal consistent even if
// Shoko's watched set changes while the host pages through it.
func (s *Server) itemsForTraversal(
	ctx context.Context,
	client *shoko.Client,
	set settings,
	traversal string,
	kind string,
) ([]*pluginv1.WatchSyncRemoteState, []string, error) {
	cacheKey := kind + "|" + traversal
	s.mu.Lock()
	if cached, ok := s.snapshots[cacheKey]; ok && time.Now().Before(cached.expires) {
		s.mu.Unlock()
		return cached.items, nil, nil
	}
	s.mu.Unlock()

	if kind == kindProgress {
		items, warnings, err := s.buildProgressItems(ctx, client, set)
		return s.cacheItems(cacheKey, items, warnings, err)
	}

	episodes, err := client.WatchedEpisodes(ctx, set.numbering == numberingTMDB)
	if err != nil {
		return nil, nil, err
	}
	series, seriesErr := client.Series(ctx)
	bySeriesID := make(map[int]shoko.Series, len(series))
	for _, entry := range series {
		bySeriesID[entry.IDs.ID] = entry
	}

	items, warnings := buildStates(episodes, bySeriesID, set.numbering)
	if seriesErr != nil {
		warnings = append(warnings, "series identities were unavailable this run, so items carry episode ids only")
	}

	s.mu.Lock()
	now := time.Now()
	for key, cached := range s.snapshots {
		if now.After(cached.expires) {
			delete(s.snapshots, key)
		}
	}
	if len(s.snapshots) >= maxSnapshots {
		var oldestKey string
		var oldest time.Time
		for key, cached := range s.snapshots {
			if oldestKey == "" || cached.expires.Before(oldest) {
				oldestKey, oldest = key, cached.expires
			}
		}
		delete(s.snapshots, oldestKey)
	}
	s.snapshots[cacheKey] = &snapshot{items: items, expires: now.Add(snapshotTTL)}
	s.mu.Unlock()

	return items, warnings, nil
}

// cacheItems stores a freshly built snapshot and returns it, applying the same
// eviction the watched traversal uses so a long-lived process cannot grow the
// map without bound.
func (s *Server) cacheItems(
	key string,
	items []*pluginv1.WatchSyncRemoteState,
	warnings []string,
	err error,
) ([]*pluginv1.WatchSyncRemoteState, []string, error) {
	if err != nil {
		return nil, nil, err
	}

	s.mu.Lock()
	now := time.Now()
	for cachedKey, cached := range s.snapshots {
		if now.After(cached.expires) {
			delete(s.snapshots, cachedKey)
		}
	}
	if len(s.snapshots) >= maxSnapshots {
		var oldestKey string
		var oldest time.Time
		for cachedKey, cached := range s.snapshots {
			if oldestKey == "" || cached.expires.Before(oldest) {
				oldestKey, oldest = cachedKey, cached.expires
			}
		}
		delete(s.snapshots, oldestKey)
	}
	s.snapshots[key] = &snapshot{items: items, expires: now.Add(snapshotTTL)}
	s.mu.Unlock()

	return items, warnings, nil
}

// requestedKind maps the state kinds the host asks for onto the traversal the
// provider runs. Watched and progress are fetched separately because they are
// different sets of records with different costs.
func requestedKind(kinds []pluginv1.WatchSyncRemoteStateKind) string {
	wantsProgress := false
	for _, kind := range kinds {
		switch kind {
		case pluginv1.WatchSyncRemoteStateKind_WATCH_SYNC_REMOTE_STATE_KIND_PROGRESS:
			wantsProgress = true
		case pluginv1.WatchSyncRemoteStateKind_WATCH_SYNC_REMOTE_STATE_KIND_WATCHED:
			return kindWatched
		}
	}
	if wantsProgress {
		return kindProgress
	}
	return kindWatched
}

// buildProgressItems reports the resume position Shoko holds for each episode.
// Only episodes part-way through count: a finished play belongs in watched
// state, and Shoko clears the position when it marks a file watched.
func (s *Server) buildProgressItems(ctx context.Context, client *shoko.Client, set settings) ([]*pluginv1.WatchSyncRemoteState, []string, error) {
	episodes, err := client.EpisodesWithFiles(ctx, set.numbering == numberingTMDB)
	if err != nil {
		return nil, nil, err
	}
	series, seriesErr := client.Series(ctx)
	bySeriesID := make(map[int]shoko.Series, len(series))
	for _, entry := range series {
		bySeriesID[entry.IDs.ID] = entry
	}

	items := make([]*pluginv1.WatchSyncRemoteState, 0, len(episodes))
	var skippedNoTimestamp, skippedNoFile, skippedFinished int
	lookups := 0

	for _, episode := range episodes {
		if episode.AniDB == nil || episode.ResumePosition.Duration <= 0 || episode.Duration.Duration <= 0 {
			continue
		}
		percent := episode.ResumePosition.Seconds() / episode.Duration.Seconds() * 100
		if percent <= 0 || percent >= scrobbleCompletionThreshold {
			skippedFinished++
			continue
		}
		season, number, ok := seasonAndEpisode(episode, set.numbering)
		if !ok {
			continue
		}
		parent, hasParent := bySeriesID[episode.IDs.ParentSeries]
		if hasParent && isMovie(parent) {
			continue
		}
		if len(episode.Files) == 0 {
			skippedNoFile++
			continue
		}
		if lookups >= maxProgressTimestampLookups {
			skippedNoTimestamp++
			continue
		}
		lookups++
		fileUserData, err := client.FileUserData(ctx, episode.Files[0].ID)
		if err != nil || fileUserData.LastUpdatedAt.IsZero() {
			skippedNoTimestamp++
			continue
		}

		externalIDs := map[string]string{}
		if len(episode.IDs.TvDB) > 0 {
			externalIDs["tvdb"] = strconv.Itoa(episode.IDs.TvDB[0])
		}
		if len(episode.IDs.TMDB.Episode) > 0 {
			externalIDs["tmdb"] = strconv.Itoa(episode.IDs.TMDB.Episode[0])
		}
		if len(episode.IDs.IMDB) > 0 {
			externalIDs["imdb"] = episode.IDs.IMDB[0]
		}
		externalIDs["shoko"] = strconv.Itoa(episode.IDs.ID)

		media := &pluginv1.WatchSyncMedia{
			MediaType:             pluginv1.WatchSyncMediaType_WATCH_SYNC_MEDIA_TYPE_EPISODE,
			Title:                 episode.Name,
			ExternalIds:           externalIDs,
			SeasonNumber:          int32(season),
			EpisodeNumber:         int32(number),
			AbsoluteEpisodeNumber: int32(episode.AniDB.EpisodeNumber),
		}
		if hasParent {
			media.SeriesTitle = parent.Name
			if ids := seriesExternalIDs(parent); len(ids) > 0 {
				media.SeriesExternalIds = ids
			}
		}

		items = append(items, &pluginv1.WatchSyncRemoteState{
			ProviderItemKey: providerItemKeyPrefix + strconv.Itoa(episode.IDs.ID),
			Media:           media,
			Progress: &pluginv1.WatchSyncRemoteProgressState{
				ProgressPercent: percent,
				PausedAt:        timestamppb.New(fileUserData.LastUpdatedAt.UTC()),
			},
		})
	}

	var warnings []string
	if seriesErr != nil {
		warnings = append(warnings, "series identities were unavailable this run, so items carry episode ids only")
	}
	if skippedNoTimestamp > 0 {
		warnings = append(warnings, fmt.Sprintf(
			"skipped %d resume positions without a usable timestamp, so they cannot be ordered against local progress",
			skippedNoTimestamp))
	}
	if skippedNoFile > 0 {
		warnings = append(warnings, fmt.Sprintf("skipped %d episodes without a file", skippedNoFile))
	}
	if skippedFinished > 0 {
		warnings = append(warnings, fmt.Sprintf("skipped %d resume positions outside the part-way range", skippedFinished))
	}
	return items, warnings, nil
}

// buildStates converts Shoko episodes into remote watched states.
func buildStates(episodes []shoko.Episode, series map[int]shoko.Series, numbering string) ([]*pluginv1.WatchSyncRemoteState, []string) {
	items := make([]*pluginv1.WatchSyncRemoteState, 0, len(episodes))
	movies := make(map[int]*pluginv1.WatchSyncRemoteState)
	var skippedNoAniDB, skippedNoNumber int

	for _, episode := range episodes {
		if episode.Watched == nil {
			continue
		}
		if episode.AniDB == nil {
			skippedNoAniDB++
			continue
		}
		season, number, ok := seasonAndEpisode(episode, numbering)
		if !ok {
			skippedNoNumber++
			continue
		}

		externalIDs := map[string]string{}
		if len(episode.IDs.TvDB) > 0 {
			externalIDs["tvdb"] = strconv.Itoa(episode.IDs.TvDB[0])
		}
		if len(episode.IDs.TMDB.Episode) > 0 {
			externalIDs["tmdb"] = strconv.Itoa(episode.IDs.TMDB.Episode[0])
		}
		if len(episode.IDs.IMDB) > 0 {
			externalIDs["imdb"] = episode.IDs.IMDB[0]
		}
		externalIDs["shoko"] = strconv.Itoa(episode.IDs.ID)

		parent, hasParent := series[episode.IDs.ParentSeries]

		// A media server stores an AniDB movie as a movie rather than as a
		// series with one episode, so the record has to be described as one.
		if hasParent && isMovie(parent) {
			movieIDs := map[string]string{"shoko": strconv.Itoa(episode.IDs.ID)}
			for namespace, value := range seriesExternalIDs(parent) {
				movieIDs[namespace] = value
			}
			// One film is one item to the media server even when Shoko files
			// more than one watched entry under it, so the entries collapse to
			// the latest play with the highest count.
			state := &pluginv1.WatchSyncRemoteState{
				ProviderItemKey: providerItemKeyPrefix + strconv.Itoa(episode.IDs.ID),
				Media: &pluginv1.WatchSyncMedia{
					MediaType:   pluginv1.WatchSyncMediaType_WATCH_SYNC_MEDIA_TYPE_MOVIE,
					Title:       parent.Name,
					ExternalIds: movieIDs,
				},
				Watched: watchedState(episode),
			}
			if existing, ok := movies[parent.IDs.ID]; ok {
				movies[parent.IDs.ID] = mergeMoviePlays(existing, state)
			} else {
				movies[parent.IDs.ID] = state
			}
			continue
		}

		media := &pluginv1.WatchSyncMedia{
			MediaType:             pluginv1.WatchSyncMediaType_WATCH_SYNC_MEDIA_TYPE_EPISODE,
			Title:                 episode.Name,
			ExternalIds:           externalIDs,
			SeasonNumber:          int32(season),
			EpisodeNumber:         int32(number),
			AbsoluteEpisodeNumber: int32(episode.AniDB.EpisodeNumber),
		}

		if hasParent {
			media.SeriesTitle = parent.Name
			if ids := seriesExternalIDs(parent); len(ids) > 0 {
				media.SeriesExternalIds = ids
			}
		}

		items = append(items, &pluginv1.WatchSyncRemoteState{
			ProviderItemKey: providerItemKeyPrefix + strconv.Itoa(episode.IDs.ID),
			Media:           media,
			Watched:         watchedState(episode),
		})
	}

	for _, seriesID := range sortedKeys(movies) {
		items = append(items, movies[seriesID])
	}

	var warnings []string
	if skippedNoAniDB > 0 {
		warnings = append(warnings, fmt.Sprintf("skipped %d watched records without an AniDB episode", skippedNoAniDB))
	}
	if skippedNoNumber > 0 {
		warnings = append(warnings, fmt.Sprintf("skipped %d watched records that are not episodes", skippedNoNumber))
	}
	return items, warnings
}

// mergeMoviePlays keeps the later of two plays of the same film, and the higher
// of the two play counts.
func mergeMoviePlays(existing, incoming *pluginv1.WatchSyncRemoteState) *pluginv1.WatchSyncRemoteState {
	merged := existing
	if incoming.GetWatched().GetLastWatchedAt().AsTime().After(existing.GetWatched().GetLastWatchedAt().AsTime()) {
		merged = incoming
	}
	count := existing.GetWatched().GetPlayCount()
	if incoming.GetWatched().GetPlayCount() > count {
		count = incoming.GetWatched().GetPlayCount()
	}
	merged.Watched.PlayCount = count
	return merged
}

// sortedKeys returns the map keys in ascending order so a rebuild produces the
// same order every time.
func sortedKeys(states map[int]*pluginv1.WatchSyncRemoteState) []int {
	keys := make([]int, 0, len(states))
	for key := range states {
		keys = append(keys, key)
	}
	sort.Ints(keys)
	return keys
}

// isMovie reports whether Shoko classifies the series as a film.
func isMovie(series shoko.Series) bool {
	return series.AniDB != nil && series.AniDB.Type == shoko.AnimeTypeMovie
}

// seriesExternalIDs returns the provider ids a media server knows the series by.
func seriesExternalIDs(series shoko.Series) map[string]string {
	ids := map[string]string{}
	if len(series.IDs.TvDB) > 0 {
		ids["tvdb"] = strconv.Itoa(series.IDs.TvDB[0])
	}
	if len(series.IDs.TMDB.Show) > 0 {
		ids["tmdb"] = strconv.Itoa(series.IDs.TMDB.Show[0])
	}
	if len(series.IDs.IMDB) > 0 {
		ids["imdb"] = series.IDs.IMDB[0]
	}
	return ids
}

// watchedState reports the last completed play. A record that reached this
// point is watched, so a stored count of zero still means one play.
func watchedState(episode shoko.Episode) *pluginv1.WatchSyncRemoteWatchedState {
	playCount := episode.WatchCount
	if playCount < 1 {
		playCount = 1
	}
	return &pluginv1.WatchSyncRemoteWatchedState{
		PlayCount:     int32(playCount),
		LastWatchedAt: timestamppb.New(episode.Watched.UTC()),
	}
}

// seasonAndEpisode returns the season and episode number a media server uses
// for the episode, or ok=false for entries that are not episodes.
func seasonAndEpisode(episode shoko.Episode, numbering string) (int, int, bool) {
	if numbering == numberingTMDB && episode.TMDB != nil {
		for _, candidate := range episode.TMDB.Episodes {
			if candidate.SeasonNumber >= 0 && candidate.EpisodeNumber > 0 {
				return candidate.SeasonNumber, candidate.EpisodeNumber, true
			}
		}
	}
	if episode.AniDB == nil {
		return 0, 0, false
	}
	switch episode.AniDB.Type {
	case shoko.EpisodeTypeEpisode:
		return 1, episode.AniDB.EpisodeNumber, true
	case shoko.EpisodeTypeSpecial:
		return 0, episode.AniDB.EpisodeNumber, true
	}
	return 0, 0, false
}

// ---------------------------------------------------------------------------
// Export
// ---------------------------------------------------------------------------

// ApplyEvents writes Silo's watched and unwatched actions into Shoko.
func (s *Server) ApplyEvents(ctx context.Context, req *pluginv1.WatchSyncApplyEventsRequest) (*pluginv1.WatchSyncApplyEventsResponse, error) {
	set, err := s.settingsFromContext(req.GetContext())
	if err != nil {
		return &pluginv1.WatchSyncApplyEventsResponse{Fault: faultMessage(err)}, nil
	}
	client, err := clientFor(set)
	if err != nil {
		return &pluginv1.WatchSyncApplyEventsResponse{Fault: faultMessage(err)}, nil
	}

	index, indexErr := s.indexFor(ctx, client, set)
	results := make([]*pluginv1.WatchSyncApplyResult, 0, len(req.GetEvents()))
	for _, event := range req.GetEvents() {
		results = append(results, s.applyEvent(ctx, client, index, indexErr, event))
	}
	return &pluginv1.WatchSyncApplyEventsResponse{Results: results}, nil
}

func (s *Server) applyEvent(ctx context.Context, client *shoko.Client, index *episodeIndex, indexErr error, event *pluginv1.WatchSyncEvent) *pluginv1.WatchSyncApplyResult {
	result := &pluginv1.WatchSyncApplyResult{EventId: event.GetEventId()}

	operation := event.GetOperation()
	switch operation {
	case pluginv1.WatchSyncOperation_WATCH_SYNC_OPERATION_MARK_WATCHED,
		pluginv1.WatchSyncOperation_WATCH_SYNC_OPERATION_MARK_UNWATCHED:
	case pluginv1.WatchSyncOperation_WATCH_SYNC_OPERATION_SCROBBLE_START,
		pluginv1.WatchSyncOperation_WATCH_SYNC_OPERATION_SCROBBLE_PAUSE,
		pluginv1.WatchSyncOperation_WATCH_SYNC_OPERATION_SCROBBLE_STOP:
		return s.applyScrobble(ctx, client, index, indexErr, event)
	default:
		result.Status = pluginv1.WatchSyncApplyStatus_WATCH_SYNC_APPLY_STATUS_REJECTED
		result.Fault = &pluginv1.WatchSyncFault{
			Code:        pluginv1.WatchSyncFaultCode_WATCH_SYNC_FAULT_CODE_INVALID_REQUEST,
			SafeMessage: "this provider stores watched state and playback progress only",
		}
		return result
	}

	if indexErr != nil {
		result.Status = pluginv1.WatchSyncApplyStatus_WATCH_SYNC_APPLY_STATUS_RETRY
		result.Fault = faultMessage(indexErr)
		return result
	}

	episodeID, ok := resolveEpisode(index, event)
	if !ok {
		result.Status = pluginv1.WatchSyncApplyStatus_WATCH_SYNC_APPLY_STATUS_REJECTED
		result.Fault = &pluginv1.WatchSyncFault{
			Code:        pluginv1.WatchSyncFaultCode_WATCH_SYNC_FAULT_CODE_PERMANENT,
			SafeMessage: "no Shoko episode matches this item",
		}
		return result
	}

	var watchedAt *time.Time
	if operation == pluginv1.WatchSyncOperation_WATCH_SYNC_OPERATION_MARK_WATCHED {
		at := time.Now().UTC()
		if event.GetOccurredAt() != nil {
			at = event.GetOccurredAt().AsTime().UTC()
		}
		watchedAt = &at
	}

	if err := client.SetWatched(ctx, episodeID, watchedAt); err != nil {
		fault := asFault(err)
		if fault.code == pluginv1.WatchSyncFaultCode_WATCH_SYNC_FAULT_CODE_PERMANENT {
			result.Status = pluginv1.WatchSyncApplyStatus_WATCH_SYNC_APPLY_STATUS_REJECTED
		} else {
			result.Status = pluginv1.WatchSyncApplyStatus_WATCH_SYNC_APPLY_STATUS_RETRY
		}
		result.Fault = &pluginv1.WatchSyncFault{Code: fault.code, SafeMessage: fault.message}
		return result
	}

	result.Status = pluginv1.WatchSyncApplyStatus_WATCH_SYNC_APPLY_STATUS_APPLIED
	return result
}

// applyScrobble writes a playback position into the file record Shoko keeps for
// the episode. A stop at or past the completion threshold is also what marks
// the file watched, so it is sent as a watched write with no position rather
// than as a position that happens to be near the end.
func (s *Server) applyScrobble(
	ctx context.Context,
	client *shoko.Client,
	index *episodeIndex,
	indexErr error,
	event *pluginv1.WatchSyncEvent,
) *pluginv1.WatchSyncApplyResult {
	result := &pluginv1.WatchSyncApplyResult{EventId: event.GetEventId()}

	if indexErr != nil {
		result.Status = pluginv1.WatchSyncApplyStatus_WATCH_SYNC_APPLY_STATUS_RETRY
		result.Fault = faultMessage(indexErr)
		return result
	}

	episodeID, ok := resolveEpisode(index, event)
	if !ok {
		result.Status = pluginv1.WatchSyncApplyStatus_WATCH_SYNC_APPLY_STATUS_REJECTED
		result.Fault = &pluginv1.WatchSyncFault{
			Code:        pluginv1.WatchSyncFaultCode_WATCH_SYNC_FAULT_CODE_PERMANENT,
			SafeMessage: "no Shoko episode matches this item",
		}
		return result
	}

	fileID, err := s.fileFor(ctx, client, episodeID)
	if err != nil {
		fault := asFault(err)
		if fault.code == pluginv1.WatchSyncFaultCode_WATCH_SYNC_FAULT_CODE_PERMANENT {
			result.Status = pluginv1.WatchSyncApplyStatus_WATCH_SYNC_APPLY_STATUS_REJECTED
		} else {
			result.Status = pluginv1.WatchSyncApplyStatus_WATCH_SYNC_APPLY_STATUS_RETRY
		}
		result.Fault = &pluginv1.WatchSyncFault{Code: fault.code, SafeMessage: fault.message}
		return result
	}

	var (
		watched  *bool
		position *time.Duration
		op       shoko.ScrobbleOperation
	)
	switch event.GetOperation() {
	case pluginv1.WatchSyncOperation_WATCH_SYNC_OPERATION_SCROBBLE_START:
		op = shoko.ScrobblePlay
	case pluginv1.WatchSyncOperation_WATCH_SYNC_OPERATION_SCROBBLE_PAUSE:
		op = shoko.ScrobblePause
	default:
		op = shoko.ScrobbleStop
	}

	if event.GetOperation() == pluginv1.WatchSyncOperation_WATCH_SYNC_OPERATION_SCROBBLE_STOP && scrobbleCompleted(event) {
		finished := true
		watched = &finished
	} else if seconds := event.GetPositionSeconds(); seconds > 0 {
		span := time.Duration(seconds * float64(time.Second))
		position = &span
	}

	if watched == nil && position == nil {
		// A start at the very beginning carries nothing Shoko would store.
		result.Status = pluginv1.WatchSyncApplyStatus_WATCH_SYNC_APPLY_STATUS_NO_CHANGE
		return result
	}

	if err := client.Scrobble(ctx, fileID, op, watched, position); err != nil {
		fault := asFault(err)
		if fault.code == pluginv1.WatchSyncFaultCode_WATCH_SYNC_FAULT_CODE_PERMANENT {
			result.Status = pluginv1.WatchSyncApplyStatus_WATCH_SYNC_APPLY_STATUS_REJECTED
		} else {
			result.Status = pluginv1.WatchSyncApplyStatus_WATCH_SYNC_APPLY_STATUS_RETRY
		}
		result.Fault = &pluginv1.WatchSyncFault{Code: fault.code, SafeMessage: fault.message}
		return result
	}

	result.Status = pluginv1.WatchSyncApplyStatus_WATCH_SYNC_APPLY_STATUS_APPLIED
	return result
}

// scrobbleCompleted reports whether a stop event represents a finished play.
func scrobbleCompleted(event *pluginv1.WatchSyncEvent) bool {
	if event.GetCompleted() {
		return true
	}
	completion := event.GetCompletionPercent()
	if completion <= 0 && event.GetDurationSeconds() > 0 {
		completion = event.GetPositionSeconds() / event.GetDurationSeconds() * 100
	}
	return completion >= scrobbleCompletionThreshold
}

// fileFor returns the Shoko file an episode plays from, cached for a few
// minutes: a playback session sends several scrobbles for the same episode.
func (s *Server) fileFor(ctx context.Context, client *shoko.Client, episodeID int) (int, error) {
	s.mu.Lock()
	if cached, ok := s.files[episodeID]; ok && time.Now().Before(cached.expires) {
		s.mu.Unlock()
		return cached.fileID, nil
	}
	s.mu.Unlock()

	episode, err := client.EpisodeByID(ctx, episodeID)
	if err != nil {
		return 0, err
	}
	if len(episode.Files) == 0 {
		return 0, newFault(pluginv1.WatchSyncFaultCode_WATCH_SYNC_FAULT_CODE_PERMANENT,
			"the Shoko episode has no file, so it has no playback position")
	}
	fileID := episode.Files[0].ID

	s.mu.Lock()
	if len(s.files) >= 512 {
		now := time.Now()
		for key, cached := range s.files {
			if now.After(cached.expires) {
				delete(s.files, key)
			}
		}
		if len(s.files) >= 512 {
			s.files = map[int]fileRef{}
		}
	}
	s.files[episodeID] = fileRef{fileID: fileID, expires: time.Now().Add(indexTTL)}
	s.mu.Unlock()

	return fileID, nil
}

// resolveEpisode finds the Shoko episode an event addresses.
func resolveEpisode(index *episodeIndex, event *pluginv1.WatchSyncEvent) (int, bool) {
	if key := strings.TrimSpace(event.GetProviderItemKey()); strings.HasPrefix(key, providerItemKeyPrefix) {
		if id, err := strconv.Atoi(strings.TrimPrefix(key, providerItemKeyPrefix)); err == nil && id > 0 {
			return id, true
		}
	}

	media := event.GetMedia()
	if media == nil {
		return 0, false
	}

	// The series ids plus the season and episode numbers survive a library
	// that carries no episode-level provider ids of its own.
	season := media.GetSeasonNumber()
	number := media.GetEpisodeNumber()
	if number > 0 {
		for _, namespace := range []string{"tvdb", "tmdb"} {
			value := strings.TrimSpace(media.GetSeriesExternalIds()[namespace])
			if value == "" {
				continue
			}
			key := namespace + ":" + value + ":" + strconv.Itoa(int(season)) + ":" + strconv.Itoa(int(number))
			if id, ok := index.bySeriesEpisode[key]; ok {
				return id, true
			}
		}
	}

	// A movie carries the ids of the film itself, not of an episode.
	for _, namespace := range []string{"tmdb", "imdb", "tvdb"} {
		value := strings.TrimSpace(media.GetExternalIds()[namespace])
		if value == "" {
			continue
		}
		if id, ok := index.byMovie[namespace+":"+value]; ok {
			return id, true
		}
	}

	for _, namespace := range []string{"tvdb", "tmdb", "imdb", "shoko"} {
		value := strings.TrimSpace(media.GetExternalIds()[namespace])
		if value == "" {
			continue
		}
		if id, ok := index.byID[namespace+":"+value]; ok {
			return id, true
		}
	}

	return 0, false
}

// indexFor returns the episode index for this Shoko server, rebuilding it when
// it is older than indexTTL.
func (s *Server) indexFor(ctx context.Context, client *shoko.Client, set settings) (*episodeIndex, error) {
	key := client.BaseURL() + "|" + set.numbering

	s.mu.Lock()
	if cached, ok := s.indexes[key]; ok && time.Since(cached.built) < indexTTL {
		s.mu.Unlock()
		return cached, nil
	}
	s.mu.Unlock()

	index, err := buildIndex(ctx, client, set)
	if err != nil {
		return nil, err
	}

	s.mu.Lock()
	if len(s.indexes) >= maxIndexes {
		for existing := range s.indexes {
			delete(s.indexes, existing)
		}
	}
	s.indexes[key] = index
	s.mu.Unlock()

	return index, nil
}

// buildIndex fetches every episode once and keys it by every id it carries.
func buildIndex(ctx context.Context, client *shoko.Client, set settings) (*episodeIndex, error) {
	episodes, err := client.Episodes(ctx, set.numbering == numberingTMDB)
	if err != nil {
		return nil, err
	}
	series, err := client.Series(ctx)
	if err != nil {
		return nil, err
	}
	bySeriesID := make(map[int]shoko.Series, len(series))
	for _, entry := range series {
		bySeriesID[entry.IDs.ID] = entry
	}

	index := &episodeIndex{
		built:           time.Now(),
		byID:            make(map[string]int, len(episodes)*3),
		bySeriesEpisode: make(map[string]int, len(episodes)*3),
		byMovie:         make(map[string]int, len(series)),
	}

	for _, episode := range episodes {
		id := episode.IDs.ID
		if id <= 0 {
			continue
		}
		remember(index.byID, "shoko:"+strconv.Itoa(id), id)
		for _, value := range episode.IDs.TvDB {
			remember(index.byID, "tvdb:"+strconv.Itoa(value), id)
		}
		for _, value := range episode.IDs.TMDB.Episode {
			remember(index.byID, "tmdb:"+strconv.Itoa(value), id)
		}
		for _, value := range episode.IDs.IMDB {
			remember(index.byID, "imdb:"+value, id)
		}

		season, number, ok := seasonAndEpisode(episode, set.numbering)
		if !ok {
			continue
		}
		parent, ok := bySeriesID[episode.IDs.ParentSeries]
		if !ok {
			continue
		}
		if isMovie(parent) {
			for namespace, value := range seriesExternalIDs(parent) {
				remember(index.byMovie, namespace+":"+value, id)
			}
			continue
		}

		suffix := ":" + strconv.Itoa(season) + ":" + strconv.Itoa(number)
		for _, value := range parent.IDs.TvDB {
			remember(index.bySeriesEpisode, "tvdb:"+strconv.Itoa(value)+suffix, id)
		}
		for _, value := range parent.IDs.TMDB.Show {
			remember(index.bySeriesEpisode, "tmdb:"+strconv.Itoa(value)+suffix, id)
		}
	}

	return index, nil
}

// remember stores the first episode seen for a key. Two Shoko episodes can
// share a cross-reference id when the provider maps them together; the lowest
// Shoko episode id wins so the choice is stable between rebuilds.
func remember(target map[string]int, key string, id int) {
	if existing, ok := target[key]; ok && existing <= id {
		return
	}
	target[key] = id
}
