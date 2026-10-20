// Package shoko is a small client for the Shoko Server REST API v3.
//
// Only the calls Silo's watch-state sync needs are implemented: the current
// user, the episode list, the series list and a partial update of one
// episode's user data.
package shoko

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"
)

// EpisodeType mirrors Shoko.Abstractions.Metadata.Enums.EpisodeType. Shoko can
// serialise it either as a number or as a name, so both are accepted.
type EpisodeType int

const (
	EpisodeTypeUnknown EpisodeType = 0
	EpisodeTypeEpisode EpisodeType = 1
	EpisodeTypeCredits EpisodeType = 2
	EpisodeTypeSpecial EpisodeType = 3
	EpisodeTypeTrailer EpisodeType = 4
	EpisodeTypeParody  EpisodeType = 5
	EpisodeTypeOther   EpisodeType = 6
)

var episodeTypeNames = map[string]EpisodeType{
	"Unknown": EpisodeTypeUnknown,
	"Episode": EpisodeTypeEpisode,
	"Normal":  EpisodeTypeEpisode,
	"Credits": EpisodeTypeCredits,
	"Special": EpisodeTypeSpecial,
	"Trailer": EpisodeTypeTrailer,
	"Parody":  EpisodeTypeParody,
	"Other":   EpisodeTypeOther,
}

// UnmarshalJSON accepts both the numeric and the named form of the enum.
func (t *EpisodeType) UnmarshalJSON(data []byte) error {
	trimmed := strings.Trim(strings.TrimSpace(string(data)), `"`)
	if trimmed == "" || trimmed == "null" {
		*t = EpisodeTypeUnknown
		return nil
	}
	if n, err := strconv.Atoi(trimmed); err == nil {
		*t = EpisodeType(n)
		return nil
	}
	if named, ok := episodeTypeNames[trimmed]; ok {
		*t = named
		return nil
	}
	*t = EpisodeTypeUnknown
	return nil
}

// ListResult is Shoko's paged envelope.
type ListResult[T any] struct {
	Total int `json:"Total"`
	List  []T `json:"List"`
}

// User is the subset of the current-user DTO the sync needs.
type User struct {
	ID       int    `json:"ID"`
	Username string `json:"Username"`
}

// SeriesIDs carries the cross-reference ids of a series.
type SeriesIDs struct {
	ID    int      `json:"ID"`
	AniDB int      `json:"AniDB"`
	TvDB  []int    `json:"TvDB"`
	IMDB  []string `json:"IMDB"`
	TMDB  struct {
		Show []int `json:"Show"`
	} `json:"TMDB"`
}

// Series is the subset of the series DTO the sync needs.
type Series struct {
	IDs   SeriesIDs   `json:"IDs"`
	Name  string      `json:"Name"`
	AniDB *AniDBAnime `json:"AniDB"`
}

// AnimeType is the AniDB classification of a series. Shoko serialises it as an
// integer.
type AnimeType int

// AnimeTypeMovie marks a self-contained story. Shoko files it as a series with
// one episode, while a media server files the same title as a movie.
const AnimeTypeMovie AnimeType = 0

// AniDBAnime is the subset of the AniDB series DTO the provider reads.
type AniDBAnime struct {
	Type AnimeType `json:"Type"`
}

// TmdbEpisode is the subset of the TMDB episode DTO used for numbering.
type TmdbEpisode struct {
	ID            int `json:"ID"`
	ShowID        int `json:"ShowID"`
	SeasonNumber  int `json:"SeasonNumber"`
	EpisodeNumber int `json:"EpisodeNumber"`
}

// AniDBEpisode is the subset of the AniDB episode DTO used for numbering.
type AniDBEpisode struct {
	ID            int         `json:"ID"`
	AnimeID       int         `json:"AnimeID"`
	Type          EpisodeType `json:"Type"`
	EpisodeNumber int         `json:"EpisodeNumber"`
}

// EpisodeIDs carries the cross-reference ids of an episode.
type EpisodeIDs struct {
	ID           int      `json:"ID"`
	ParentSeries int      `json:"ParentSeries"`
	AniDB        int      `json:"AniDB"`
	TvDB         []int    `json:"TvDB"`
	IMDB         []string `json:"IMDB"`
	TMDB         struct {
		Episode []int `json:"Episode"`
		Movie   []int `json:"Movie"`
		Show    []int `json:"Show"`
	} `json:"TMDB"`
}

// Episode is the subset of the episode DTO the sync needs.
type Episode struct {
	IDs        EpisodeIDs       `json:"IDs"`
	Name       string           `json:"Name"`
	Watched    *time.Time       `json:"Watched"`
	WatchCount int              `json:"WatchCount"`
	AniDB      *AniDBEpisode    `json:"AniDB"`
	TMDB       *TmdbEpisodeData `json:"TMDB"`

	// Duration is the runtime of the episode's file.
	Duration TimeSpan `json:"Duration"`
	// ResumePosition is the position the viewer stopped at, when Shoko holds
	// one for the episode's file.
	ResumePosition TimeSpan `json:"ResumePosition"`
	// Files is populated only when the request asks for files.
	Files []File `json:"Files"`
}

// File is the subset of the file DTO the sync needs: the identity that watch
// state and resume position are stored against inside Shoko.
type File struct {
	ID int `json:"ID"`
}

// FileUserData is the per-user record Shoko keeps for one file. Resume position
// lives here rather than on the episode, and LastUpdatedAt is the only
// timestamp Shoko offers for it.
type FileUserData struct {
	ProgressPosition TimeSpan   `json:"ProgressPosition"`
	WatchedCount     int        `json:"WatchedCount"`
	LastWatchedAt    *time.Time `json:"LastWatchedAt"`
	LastUpdatedAt    time.Time  `json:"LastUpdatedAt"`
}

// TimeSpan is Shoko's serialised System.TimeSpan, which Newtonsoft writes as
// "[-][d.]hh:mm:ss[.fffffff]". It also accepts a bare tick count or a number of
// seconds, because the API is inconsistent about the two.
type TimeSpan struct {
	Duration time.Duration
}

// UnmarshalJSON accepts the string form and a numeric tick count.
func (t *TimeSpan) UnmarshalJSON(data []byte) error {
	t.Duration = 0
	trimmed := strings.Trim(strings.TrimSpace(string(data)), `"`)
	if trimmed == "" || trimmed == "null" {
		return nil
	}
	if ticks, err := strconv.ParseInt(trimmed, 10, 64); err == nil {
		t.Duration = time.Duration(ticks) * 100 * time.Nanosecond
		return nil
	}
	parsed, err := parseTimeSpan(trimmed)
	if err != nil {
		return nil
	}
	t.Duration = parsed
	return nil
}

func parseTimeSpan(value string) (time.Duration, error) {
	negative := strings.HasPrefix(value, "-")
	value = strings.TrimPrefix(value, "-")

	days := 0
	if dot := strings.Index(value, "."); dot >= 0 {
		rest := value[dot+1:]
		// A dot only introduces days when another colon follows it; otherwise
		// it separates fractional seconds.
		if strings.Contains(rest, ":") {
			parsedDays, err := strconv.Atoi(value[:dot])
			if err != nil {
				return 0, fmt.Errorf("parse timespan days: %w", err)
			}
			days = parsedDays
			value = rest
		}
	}

	parts := strings.Split(value, ":")
	if len(parts) > 3 || len(parts) < 1 {
		return 0, fmt.Errorf("parse timespan %q", value)
	}
	seconds := 0.0
	for _, part := range parts {
		parsed, err := strconv.ParseFloat(part, 64)
		if err != nil {
			return 0, fmt.Errorf("parse timespan %q: %w", value, err)
		}
		seconds = seconds*60 + parsed
	}

	total := time.Duration(days)*24*time.Hour + time.Duration(seconds*float64(time.Second))
	if negative {
		total = -total
	}
	return total, nil
}

// Seconds returns the span as whole seconds, truncated.
func (t TimeSpan) Seconds() float64 { return t.Duration.Seconds() }

// TmdbEpisodeData is the TMDB side data Shoko attaches when TMDB is requested.
type TmdbEpisodeData struct {
	Episodes []TmdbEpisode `json:"Episodes"`
}

// ErrUnauthorized reports a rejected API key.
var ErrUnauthorized = errors.New("shoko rejected the API key")

// ErrNotFound reports that Shoko does not know the requested record.
var ErrNotFound = errors.New("shoko has no such record")

// ErrRateLimited reports that Shoko asked the caller to slow down.
var ErrRateLimited = errors.New("shoko rate limited the request")

// APIError is a non-2xx response from Shoko.
type APIError struct {
	StatusCode int
	// Detail is the start of the response body. It is for local diagnosis
	// only: it is provider output and must never reach an operator-facing
	// message.
	Detail string
}

func (e *APIError) Error() string {
	return fmt.Sprintf("shoko returned HTTP %d", e.StatusCode)
}

// Client talks to one Shoko Server instance as one API key.
type Client struct {
	baseURL string
	apiKey  string
	http    *http.Client
}

// New validates the base URL and returns a client. The API key may be empty
// only for the calls that do not need one.
func New(baseURL, apiKey string) (*Client, error) {
	baseURL = strings.TrimRight(strings.TrimSpace(baseURL), "/")
	parsed, err := url.Parse(baseURL)
	if err != nil || parsed.Host == "" {
		return nil, errors.New("the Shoko server URL must be an absolute URL")
	}
	if parsed.Scheme != "http" && parsed.Scheme != "https" {
		return nil, errors.New("the Shoko server URL must use http or https")
	}
	return &Client{
		baseURL: baseURL,
		apiKey:  strings.TrimSpace(apiKey),
		http:    &http.Client{Timeout: 90 * time.Second},
	}, nil
}

// BaseURL is the normalised server URL this client talks to.
func (c *Client) BaseURL() string { return c.baseURL }

func (c *Client) send(ctx context.Context, method, path string, query url.Values, body []byte, contentType string, out any) error {
	endpoint := c.baseURL + path
	if len(query) > 0 {
		endpoint += "?" + query.Encode()
	}
	var reader io.Reader
	if body != nil {
		reader = bytes.NewReader(body)
	}
	req, err := http.NewRequestWithContext(ctx, method, endpoint, reader)
	if err != nil {
		return fmt.Errorf("build shoko request: %w", err)
	}
	req.Header.Set("Accept", "application/json")
	if c.apiKey != "" {
		req.Header.Set("apikey", c.apiKey)
	}
	if contentType != "" {
		req.Header.Set("Content-Type", contentType)
	}

	resp, err := c.http.Do(req)
	if err != nil {
		return fmt.Errorf("call shoko: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		// Keep a short prefix of the body for local diagnosis only.
		detail, _ := io.ReadAll(io.LimitReader(resp.Body, 2048))
		apiErr := &APIError{StatusCode: resp.StatusCode, Detail: strings.TrimSpace(string(detail))}
		switch resp.StatusCode {
		case http.StatusUnauthorized, http.StatusForbidden:
			return fmt.Errorf("%w: %w", ErrUnauthorized, apiErr)
		case http.StatusNotFound:
			return fmt.Errorf("%w: %w", ErrNotFound, apiErr)
		case http.StatusTooManyRequests:
			return fmt.Errorf("%w: %w", ErrRateLimited, apiErr)
		}
		return apiErr
	}

	if out == nil {
		_, _ = io.Copy(io.Discard, resp.Body)
		return nil
	}
	if err := json.NewDecoder(resp.Body).Decode(out); err != nil {
		return fmt.Errorf("decode shoko response: %w", err)
	}
	return nil
}

// CurrentUser returns the account the API key belongs to.
func (c *Client) CurrentUser(ctx context.Context) (*User, error) {
	var user User
	if err := c.send(ctx, http.MethodGet, "/api/v3/User/Current", nil, nil, "", &user); err != nil {
		return nil, err
	}
	return &user, nil
}

const maxPages = 500

// fetchAll walks every page of a list endpoint.
const episodePageSize = 1000

func fetchAll[T any](ctx context.Context, c *Client, path string, query url.Values, pageSize int) ([]T, error) {
	var all []T
	for page := 1; page <= maxPages; page++ {
		q := url.Values{}
		for key, values := range query {
			q[key] = append([]string(nil), values...)
		}
		q.Set("pageSize", strconv.Itoa(pageSize))
		q.Set("page", strconv.Itoa(page))

		var result ListResult[T]
		if err := c.send(ctx, http.MethodGet, path, q, nil, "", &result); err != nil {
			return nil, err
		}
		all = append(all, result.List...)
		if len(result.List) < pageSize || len(all) >= result.Total {
			return all, nil
		}
	}
	return nil, fmt.Errorf("shoko pagination exceeded %d pages", maxPages)
}

// seriesPageSize is the largest page the Series endpoint accepts. It caps at
// 100 where the Episode endpoint allows 1000, so the two cannot share a size.
const seriesPageSize = 100

// Series returns every series the API key can see.
func (c *Client) Series(ctx context.Context) ([]Series, error) {
	// The AniDB type decides whether an episode is a movie to the media server,
	// and it only travels with the AniDB side data.
	query := url.Values{}
	query.Set("includeDataFrom", "AniDB")
	return fetchAll[Series](ctx, c, "/api/v3/Series", query, seriesPageSize)
}

// Episodes returns every episode the API key can see, watched or not.
func (c *Client) Episodes(ctx context.Context, includeTmdb bool) ([]Episode, error) {
	return fetchAll[Episode](ctx, c, "/api/v3/Episode", episodeQuery(includeTmdb), episodePageSize)
}

// WatchedEpisodes returns only the episodes the user has completed.
func (c *Client) WatchedEpisodes(ctx context.Context, includeTmdb bool) ([]Episode, error) {
	query := episodeQuery(includeTmdb)
	query.Set("includeWatched", "Only")
	return fetchAll[Episode](ctx, c, "/api/v3/Episode", query, episodePageSize)
}

// episodeQuery asks for the side data the provider needs: AniDB always, because
// the episode number, the episode type and the absolute number only travel with
// AniDB details, and TMDB when the connection numbers episodes by TMDB season.
func episodeQuery(includeTmdb bool) url.Values {
	query := url.Values{}
	sources := []string{"AniDB"}
	if includeTmdb {
		sources = append(sources, "TMDB")
	}
	query.Set("includeDataFrom", strings.Join(sources, ","))
	return query
}

// EpisodesWithFiles returns every episode with its files attached. Resume
// position lives on the file record, so the file ids are needed to report it.
func (c *Client) EpisodesWithFiles(ctx context.Context, includeTmdb bool) ([]Episode, error) {
	query := episodeQuery(includeTmdb)
	query.Set("includeFiles", "true")
	return fetchAll[Episode](ctx, c, "/api/v3/Episode", query, episodePageSize)
}

// SetWatched records a completed play at the given time, or clears it when
// watchedAt is nil.
//
// It sends a JSON patch that touches only LastPlayedAt. The controller starts
// from the user's existing record, so every other field (play count, favourite,
// tags, rating) is carried through unchanged, and rewriting the same timestamp
// is a no-op rather than a duplicate play.
func (c *Client) SetWatched(ctx context.Context, episodeID int, watchedAt *time.Time) error {
	var value any
	if watchedAt != nil {
		utc := watchedAt.UTC()
		value = utc.Format("2006-01-02T15:04:05.000Z")
	}
	patch := []map[string]any{{"op": "add", "path": "/LastPlayedAt", "value": value}}
	body, err := json.Marshal(patch)
	if err != nil {
		return fmt.Errorf("encode shoko patch: %w", err)
	}
	path := "/api/v3/Episode/" + strconv.Itoa(episodeID) + "/UserData"
	return c.send(ctx, http.MethodPatch, path, nil, body, "application/json-patch+json", nil)
}

// EpisodeByID returns one episode with the files attached, which is where the
// file id that resume position is stored against comes from.
func (c *Client) EpisodeByID(ctx context.Context, episodeID int) (*Episode, error) {
	query := url.Values{}
	query.Set("includeFiles", "true")
	query.Set("includeDataFrom", "AniDB")
	var episode Episode
	path := "/api/v3/Episode/" + strconv.Itoa(episodeID)
	if err := c.send(ctx, http.MethodGet, path, query, nil, "", &episode); err != nil {
		return nil, err
	}
	return &episode, nil
}

// FileUserData returns the caller's resume position and its timestamp for one
// file.
func (c *Client) FileUserData(ctx context.Context, fileID int) (*FileUserData, error) {
	var userData FileUserData
	path := "/api/v3/File/" + strconv.Itoa(fileID) + "/UserData"
	if err := c.send(ctx, http.MethodGet, path, nil, nil, "", &userData); err != nil {
		return nil, err
	}
	return &userData, nil
}

// ScrobbleOperation names the playback transition Shoko records. Shoko treats
// the four the provider sends differently: a stop at or past the completion
// threshold is what marks a file watched.
type ScrobbleOperation string

// The scrobble events the provider forwards.
const (
	ScrobblePlay   ScrobbleOperation = "play"
	ScrobbleResume ScrobbleOperation = "resume"
	ScrobblePause  ScrobbleOperation = "pause"
	ScrobbleStop   ScrobbleOperation = "stop"
)

// Scrobble reports playback progress for one file. resumePosition is nil when
// the position should be left alone; watched is nil when the watched flag
// should be left alone.
func (c *Client) Scrobble(
	ctx context.Context,
	fileID int,
	operation ScrobbleOperation,
	watched *bool,
	resumePosition *time.Duration,
) error {
	query := url.Values{}
	query.Set("event", string(operation))
	if watched != nil {
		query.Set("watched", strconv.FormatBool(*watched))
	}
	if resumePosition != nil {
		// Shoko reads a bare integer as .NET ticks, which are 100ns each.
		query.Set("resumePosition", strconv.FormatInt(int64(*resumePosition/100), 10))
	}
	path := "/api/v3/File/" + strconv.Itoa(fileID) + "/Scrobble"
	return c.send(ctx, http.MethodPatch, path, query, nil, "", nil)
}

// Stats is a cheap summary of what an API key can see. Each field costs one
// request with a page size of one, so the numbers come from the response
// envelope rather than from a full listing.
type Stats struct {
	Series   int
	Episodes int
	Watched  int
}

// Stats counts the series, episodes and watched episodes the key can see.
func (c *Client) Stats(ctx context.Context) (Stats, error) {
	var stats Stats
	series, err := c.total(ctx, "/api/v3/Series", nil)
	if err != nil {
		return stats, err
	}
	stats.Series = series
	episodes, err := c.total(ctx, "/api/v3/Episode", nil)
	if err != nil {
		return stats, err
	}
	stats.Episodes = episodes
	watchedQuery := url.Values{}
	watchedQuery.Set("includeWatched", "Only")
	watched, err := c.total(ctx, "/api/v3/Episode", watchedQuery)
	if err != nil {
		return stats, err
	}
	stats.Watched = watched
	return stats, nil
}

func (c *Client) total(ctx context.Context, path string, query url.Values) (int, error) {
	q := url.Values{}
	for key, values := range query {
		q[key] = append([]string(nil), values...)
	}
	q.Set("pageSize", "1")
	q.Set("page", "1")
	var result ListResult[json.RawMessage]
	if err := c.send(ctx, http.MethodGet, path, q, nil, "", &result); err != nil {
		return 0, err
	}
	return result.Total, nil
}
