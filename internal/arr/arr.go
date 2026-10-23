// Package arr is a client for the parts of the Radarr and Sonarr v3 API that
// a manual import of a queued download takes: the queue, the manual import
// listing and reprocessing, and commands.
package arr

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
)

// Kind is the application a Client talks to.
type Kind string

const (
	Radarr Kind = "radarr"
	Sonarr Kind = "sonarr"
)

// Client calls one Radarr or Sonarr instance.
type Client struct {
	Kind Kind
	// URL is the instance's base URL, without /api/v3.
	URL  string
	Key  string
	HTTP *http.Client
}

// Rejection is the reason the application gives for not importing a file.
type Rejection struct {
	Reason string `json:"reason"`
	Type   string `json:"type"`
}

// StatusMessage is a queue record's warning: Title names a file or the
// download, Messages are the reasons.
type StatusMessage struct {
	Title    string   `json:"title"`
	Messages []string `json:"messages"`
}

// Episode is a Sonarr episode. AbsoluteEpisodeNumber is 0 when the series
// has none; AirDate is YYYY-MM-DD or "".
type Episode struct {
	ID                    int    `json:"id"`
	SeasonNumber          int    `json:"seasonNumber"`
	EpisodeNumber         int    `json:"episodeNumber"`
	AbsoluteEpisodeNumber int    `json:"absoluteEpisodeNumber"`
	Title                 string `json:"title"`
	AirDate               string `json:"airDate"`
}

// QueueRecord is a queue entry. Radarr returns one per download, Sonarr one
// per episode of a download. Quality and Languages are the application's
// parse of the grabbed release, kept as sent so they can be sent back.
type QueueRecord struct {
	ID                    int             `json:"id"`
	MovieID               int             `json:"movieId"`
	SeriesID              int             `json:"seriesId"`
	EpisodeID             int             `json:"episodeId"`
	SeasonNumber          int             `json:"seasonNumber"`
	Episode               *Episode        `json:"episode"`
	Title                 string          `json:"title"`
	Status                string          `json:"status"`
	TrackedDownloadStatus string          `json:"trackedDownloadStatus"`
	TrackedDownloadState  string          `json:"trackedDownloadState"`
	StatusMessages        []StatusMessage `json:"statusMessages"`
	DownloadID            string          `json:"downloadId"`
	EpisodeHasFile        bool            `json:"episodeHasFile"`
	OutputPath            string          `json:"outputPath"`
	Quality               json.RawMessage `json:"quality"`
	Languages             json.RawMessage `json:"languages"`
}

// Ref is the id of a movie or series a file is matched to.
type Ref struct {
	ID    int    `json:"id"`
	Title string `json:"title"`
	Year  int    `json:"year"`
}

// ImportItem is a file as the manual import listing and reprocessing return
// it. Movie and Series are nil when the application matched none.
type ImportItem struct {
	ID           int             `json:"id"`
	Path         string          `json:"path"`
	RelativePath string          `json:"relativePath"`
	FolderName   string          `json:"folderName"`
	Size         int64           `json:"size"`
	Movie        *Ref            `json:"movie"`
	Series       *Ref            `json:"series"`
	SeasonNumber *int            `json:"seasonNumber"`
	Episodes     []Episode       `json:"episodes"`
	Quality      json.RawMessage `json:"quality"`
	Languages    json.RawMessage `json:"languages"`
	ReleaseGroup string          `json:"releaseGroup"`
	DownloadID   string          `json:"downloadId"`
	IndexerFlags int             `json:"indexerFlags"`
	ReleaseType  string          `json:"releaseType"`
	Rejections   []Rejection     `json:"rejections"`
}

// Reprocess is a file with the target and attributes to evaluate it with.
// MovieID is Radarr's; SeriesID, SeasonNumber, EpisodeIDs and ReleaseType
// are Sonarr's.
type Reprocess struct {
	ID           int             `json:"id,omitempty"`
	Path         string          `json:"path"`
	MovieID      int             `json:"movieId,omitempty"`
	SeriesID     int             `json:"seriesId,omitempty"`
	SeasonNumber *int            `json:"seasonNumber,omitempty"`
	EpisodeIDs   []int           `json:"episodeIds,omitempty"`
	Quality      json.RawMessage `json:"quality"`
	Languages    json.RawMessage `json:"languages"`
	ReleaseGroup string          `json:"releaseGroup,omitempty"`
	DownloadID   string          `json:"downloadId"`
	IndexerFlags int             `json:"indexerFlags"`
	ReleaseType  string          `json:"releaseType,omitempty"`
}

// ImportFile is a file of a ManualImport command. MovieID is Radarr's;
// SeriesID, EpisodeIDs and ReleaseType are Sonarr's.
type ImportFile struct {
	Path         string          `json:"path"`
	FolderName   string          `json:"folderName,omitempty"`
	MovieID      int             `json:"movieId,omitempty"`
	SeriesID     int             `json:"seriesId,omitempty"`
	EpisodeIDs   []int           `json:"episodeIds,omitempty"`
	Quality      json.RawMessage `json:"quality"`
	Languages    json.RawMessage `json:"languages"`
	ReleaseGroup string          `json:"releaseGroup,omitempty"`
	IndexerFlags int             `json:"indexerFlags"`
	ReleaseType  string          `json:"releaseType,omitempty"`
	DownloadID   string          `json:"downloadId"`
}

// Command is a queued or finished command.
type Command struct {
	ID      int    `json:"id"`
	Name    string `json:"name"`
	Status  string `json:"status"`
	Result  string `json:"result"`
	Message string `json:"message"`
}

// Finished reports whether the command will not change status again.
func (c Command) Finished() bool {
	switch c.Status {
	case "completed", "failed", "aborted", "cancelled", "orphaned":
		return true
	}
	return false
}

// Error is a non-2xx answer.
type Error struct {
	Method, Path string
	Status       int
	Body         string
}

func (e *Error) Error() string {
	return fmt.Sprintf("%s %s: %d %s: %s", e.Method, e.Path, e.Status, http.StatusText(e.Status), e.Body)
}

const pageSize = 200

// Queue returns every queue record, Sonarr's with their episodes.
func (c *Client) Queue(ctx context.Context) ([]QueueRecord, error) {
	var all []QueueRecord
	for page := 1; ; page++ {
		q := url.Values{"page": {strconv.Itoa(page)}, "pageSize": {strconv.Itoa(pageSize)}}
		if c.Kind == Sonarr {
			q.Set("includeEpisode", "true")
		}
		var res struct {
			TotalRecords int           `json:"totalRecords"`
			Records      []QueueRecord `json:"records"`
		}
		if err := c.do(ctx, http.MethodGet, "/api/v3/queue", q, nil, &res); err != nil {
			return nil, err
		}
		all = append(all, res.Records...)
		if len(res.Records) == 0 || len(all) >= res.TotalRecords {
			return all, nil
		}
	}
}

// ManualImport lists the files of the download as a manual import would,
// with the application's match of each and its reasons not to import it.
// It names no movie or series: given a seriesId with a downloadId, Sonarr
// lists the files of another download of the series.
func (c *Client) ManualImport(ctx context.Context, downloadID string) ([]ImportItem, error) {
	q := url.Values{"downloadId": {downloadID}, "filterExistingFiles": {"true"}}
	var items []ImportItem
	err := c.do(ctx, http.MethodGet, "/api/v3/manualimport", q, nil, &items)
	return items, err
}

// Reprocess evaluates the files with the given targets and attributes and
// returns them as the application would import them, with its rejections.
func (c *Client) Reprocess(ctx context.Context, files []Reprocess) ([]ImportItem, error) {
	var items []ImportItem
	err := c.do(ctx, http.MethodPost, "/api/v3/manualimport", nil, files, &items)
	return items, err
}

// StartManualImport queues a ManualImport command for the files, which the
// application imports without checking them again.
func (c *Client) StartManualImport(ctx context.Context, files []ImportFile) (Command, error) {
	var cmd Command
	err := c.do(ctx, http.MethodPost, "/api/v3/command", nil, map[string]any{
		"name":       "ManualImport",
		"files":      files,
		"importMode": "auto",
	}, &cmd)
	return cmd, err
}

// Movie is a Radarr movie.
type Movie struct {
	ID      int  `json:"id"`
	HasFile bool `json:"hasFile"`
}

// Movie returns the Radarr movie.
func (c *Client) Movie(ctx context.Context, id int) (Movie, error) {
	var m Movie
	err := c.do(ctx, http.MethodGet, "/api/v3/movie/"+strconv.Itoa(id), nil, nil, &m)
	return m, err
}

// Command returns the command's current state.
func (c *Client) Command(ctx context.Context, id int) (Command, error) {
	var cmd Command
	err := c.do(ctx, http.MethodGet, "/api/v3/command/"+strconv.Itoa(id), nil, nil, &cmd)
	return cmd, err
}

func (c *Client) do(ctx context.Context, method, path string, q url.Values, in, out any) error {
	u := strings.TrimRight(c.URL, "/") + path
	if len(q) > 0 {
		u += "?" + q.Encode()
	}
	var body io.Reader
	if in != nil {
		b, err := json.Marshal(in)
		if err != nil {
			return err
		}
		body = bytes.NewReader(b)
	}
	req, err := http.NewRequestWithContext(ctx, method, u, body)
	if err != nil {
		return err
	}
	req.Header.Set("X-Api-Key", c.Key)
	req.Header.Set("Accept", "application/json")
	if in != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	res, err := c.HTTP.Do(req)
	if err != nil {
		return err
	}
	defer res.Body.Close()
	if res.StatusCode/100 != 2 {
		msg, _ := io.ReadAll(io.LimitReader(res.Body, 1000))
		return &Error{Method: method, Path: path, Status: res.StatusCode, Body: strings.TrimSpace(string(msg))}
	}
	if err := json.NewDecoder(res.Body).Decode(out); err != nil {
		return fmt.Errorf("%s %s: decode: %w", method, path, err)
	}
	return nil
}

// KnownQuality reports whether a quality as the application sends it names
// a quality other than Unknown.
func KnownQuality(raw json.RawMessage) bool {
	var q struct {
		Quality *struct {
			ID int `json:"id"`
		} `json:"quality"`
	}
	return json.Unmarshal(raw, &q) == nil && q.Quality != nil && q.Quality.ID != 0
}

// KnownLanguages reports whether a language list as the application sends
// it names a language other than Unknown.
func KnownLanguages(raw json.RawMessage) bool {
	var ls []struct {
		ID int `json:"id"`
	}
	if json.Unmarshal(raw, &ls) != nil {
		return false
	}
	for _, l := range ls {
		if l.ID != 0 {
			return true
		}
	}
	return false
}

// UnknownQuality is the quality the application reads as "not chosen".
var UnknownQuality = json.RawMessage(`{"quality":{"id":0,"name":"Unknown"},"revision":{"version":1,"real":0,"isRepack":false}}`)
