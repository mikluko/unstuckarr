// Package unstuck finds downloads Radarr and Sonarr completed but will not
// import because they cannot tell what the files are, decides which movie or
// episodes each file is, and imports them through the application's own
// manual import.
package unstuck

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"slices"
	"strings"
	"sync"
	"time"

	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/codes"
	"go.opentelemetry.io/otel/metric"
	"go.opentelemetry.io/otel/trace"

	"github.com/mikluko/unstuckarr/internal/arr"
	"github.com/mikluko/unstuckarr/internal/decide"
	"github.com/mikluko/unstuckarr/internal/typesafe"
)

const scope = "github.com/mikluko/unstuckarr/internal/unstuck"

// API is the part of a Radarr or Sonarr client the Engine uses; *arr.Client
// is one.
type API interface {
	Queue(ctx context.Context) ([]arr.QueueRecord, error)
	ManualImport(ctx context.Context, downloadID string) ([]arr.ImportItem, error)
	Reprocess(ctx context.Context, files []arr.Reprocess) ([]arr.ImportItem, error)
	StartManualImport(ctx context.Context, files []arr.ImportFile) (arr.Command, error)
	Command(ctx context.Context, id int) (arr.Command, error)
	Movie(ctx context.Context, id int) (arr.Movie, error)
}

// Asker answers choice questions; *typesafe.Client is one.
type Asker interface {
	Ask(ctx context.Context, state any, qs map[string]typesafe.Question) (map[string]typesafe.Answer, error)
}

// Decisions on a download.
const (
	// Import is a download whose files are to be imported.
	Import = "import"
	// LeftAlone is a download unstuckarr will not import, for its Reasons.
	LeftAlone = "left-alone"
	// Waiting is a download the application is importing, or will import
	// itself.
	Waiting = "waiting"
	// Failed is a download whose decision a request failed.
	Failed = "error"
)

// Import outcomes besides the command statuses of the application.
const (
	DryRun    = "dry-run"
	Abandoned = "abandoned"
)

// File is a file of a download and what became of it.
type File struct {
	Path       string `json:"path"`
	Size       int64  `json:"size"`
	MovieID    int    `json:"movieId,omitempty"`
	EpisodeIDs []int  `json:"episodeIds,omitempty"`
	// Source is how its movie or episodes were found.
	Source string `json:"source,omitempty"`
	// Skipped is why it is not imported, or "".
	Skipped string `json:"skipped,omitempty"`
	// Unreprocessed is a file the application could not reprocess, imported
	// on the listing's rejections alone.
	Unreprocessed bool `json:"unreprocessed,omitempty"`
}

// Outcome is what became of an import.
type Outcome struct {
	CommandID int    `json:"commandId,omitempty"`
	Status    string `json:"status"`
	Result    string `json:"result,omitempty"`
	Message   string `json:"message,omitempty"`
}

// Item is the decision on a download.
type Item struct {
	App        arr.Kind `json:"app"`
	DownloadID string   `json:"downloadId"`
	Title      string   `json:"title"`
	State      string   `json:"state"`
	Decision   string   `json:"decision"`
	Reasons    []string `json:"reasons,omitempty"`
	Files      []File   `json:"files,omitempty"`
	Import     *Outcome `json:"import,omitempty"`
}

// Run is one pass over an application's queue.
type Run struct {
	App      arr.Kind  `json:"app"`
	Started  time.Time `json:"started"`
	Finished time.Time `json:"finished"`
	Error    string    `json:"error,omitempty"`
	Items    []*Item   `json:"items"`
}

// Engine decides and imports. Its methods are safe for concurrent use; at
// most one decision or import of a download is under way at a time.
type Engine struct {
	// Apply imports; otherwise decisions are logged and reported only.
	Apply bool
	// MinSize is the size below which a file is not imported.
	MinSize int64
	// LLM, when set, matches files the rules cannot.
	LLM Asker
	// MinConfidence is the confidence below which an LLM answer is no match.
	MinConfidence float64
	// Budget is reported in the status; the LLM spends it.
	Budget *typesafe.Budget
	// CommandPoll is how often a started import is checked on, and
	// CommandTimeout how long it is waited for.
	CommandPoll    time.Duration
	CommandTimeout time.Duration
	Log            *slog.Logger
	// Meter records metrics; the global MeterProvider when nil.
	Meter metric.MeterProvider

	mu      sync.Mutex
	runs    map[arr.Kind]*Run
	imports []*Item
	busy    map[string]bool
	pollers sync.WaitGroup

	metricsOnce sync.Once
	decisions   metric.Int64Counter
	outcomes    metric.Int64Counter
}

// keptImports is how many started imports the status keeps.
const keptImports = 50

func (e *Engine) metrics() {
	e.metricsOnce.Do(func() {
		mp := e.Meter
		if mp == nil {
			mp = otel.GetMeterProvider()
		}
		m := mp.Meter(scope)
		var err error
		if e.decisions, err = m.Int64Counter("unstuckarr.decisions", metric.WithUnit("{download}"),
			metric.WithDescription("Decisions on stuck downloads, by application and decision.")); err != nil {
			otel.Handle(err)
		}
		if e.outcomes, err = m.Int64Counter("unstuckarr.imports", metric.WithUnit("{import}"),
			metric.WithDescription("Finished imports, by application and command status.")); err != nil {
			otel.Handle(err)
		}
	})
}

func (e *Engine) tracer() trace.Tracer { return otel.Tracer(scope) }

// Loop runs over the application's queue at start, every interval, and on
// each trigger, until ctx ends.
func (e *Engine) Loop(ctx context.Context, kind arr.Kind, api API, every time.Duration, trigger <-chan struct{}) {
	t := time.NewTicker(every)
	defer t.Stop()
	for {
		e.Run(ctx, kind, api)
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		case <-trigger:
		}
	}
}

// Wait returns once the started imports are no longer checked on.
func (e *Engine) Wait() { e.pollers.Wait() }

// Status is what the Engine reports.
type Status struct {
	Apply   bool              `json:"apply"`
	Runs    map[arr.Kind]*Run `json:"runs"`
	Imports []*Item           `json:"imports"`
	LLM     *typesafe.Usage   `json:"llm,omitempty"`
}

// StatusJSON returns the last run of each application, the recent imports
// and the LLM's spending, as JSON.
func (e *Engine) StatusJSON() ([]byte, error) {
	e.mu.Lock()
	defer e.mu.Unlock()
	s := Status{Apply: e.Apply, Runs: e.runs, Imports: e.imports}
	if s.Runs == nil {
		s.Runs = map[arr.Kind]*Run{}
	}
	if s.Imports == nil {
		s.Imports = []*Item{}
	}
	if e.LLM != nil && e.Budget != nil {
		u := e.Budget.Usage()
		s.LLM = &u
	}
	return json.Marshal(s)
}

func (e *Engine) lock(key string) bool {
	e.mu.Lock()
	defer e.mu.Unlock()
	if e.busy == nil {
		e.busy = map[string]bool{}
	}
	if e.busy[key] {
		return false
	}
	e.busy[key] = true
	return true
}

func (e *Engine) unlock(key string) {
	e.mu.Lock()
	defer e.mu.Unlock()
	delete(e.busy, key)
}

// Run decides on every stuck download in the application's queue and, with
// Apply, starts their imports. A failure on one download is recorded on its
// Item and does not stop the others.
func (e *Engine) Run(ctx context.Context, kind arr.Kind, api API) *Run {
	e.metrics()
	ctx, span := e.tracer().Start(ctx, "unstuckarr.run", trace.WithAttributes(attribute.String("unstuckarr.app", string(kind))))
	defer span.End()
	run := &Run{App: kind, Started: time.Now(), Items: []*Item{}}
	records, err := api.Queue(ctx)
	if err != nil {
		run.Error = err.Error()
		span.SetStatus(codes.Error, err.Error())
		e.Log.ErrorContext(ctx, "queue", "app", kind, "err", err)
	} else {
		groups := group(records)
		claims := claimed(kind, groups)
		for _, g := range groups {
			if !stuck(g[0].TrackedDownloadState) {
				continue
			}
			it := e.item(ctx, kind, api, g, claims)
			run.Items = append(run.Items, it)
			e.decisions.Add(ctx, 1, metric.WithAttributes(attribute.String("unstuckarr.app", string(kind)), attribute.String("unstuckarr.decision", it.Decision)))
			e.logItem(ctx, it)
		}
	}
	run.Finished = time.Now()
	e.mu.Lock()
	if e.runs == nil {
		e.runs = map[arr.Kind]*Run{}
	}
	e.runs[kind] = run
	e.mu.Unlock()
	return run
}

func (e *Engine) logItem(ctx context.Context, it *Item) {
	attrs := []any{"app", it.App, "downloadId", it.DownloadID, "title", it.Title, "decision", it.Decision, "reasons", it.Reasons, "files", it.Files}
	if it.Import != nil {
		e.mu.Lock()
		attrs = append(attrs, "import", it.Import.Status)
		e.mu.Unlock()
	}
	e.Log.InfoContext(ctx, "decision", attrs...)
}

func stuck(state string) bool { return state == "importPending" || state == "importBlocked" }

func pending(state string) bool { return stuck(state) || state == "importing" }

// group returns the records by download, in queue order.
func group(records []arr.QueueRecord) [][]arr.QueueRecord {
	var out [][]arr.QueueRecord
	at := map[string]int{}
	for _, r := range records {
		if r.DownloadID == "" {
			continue
		}
		i, ok := at[r.DownloadID]
		if !ok {
			i = len(out)
			at[r.DownloadID] = i
			out = append(out, nil)
		}
		out[i] = append(out[i], r)
	}
	return out
}

// claimed maps each movie or episode to the downloads of it awaiting or
// under import.
func claimed(kind arr.Kind, groups [][]arr.QueueRecord) map[int]map[string]bool {
	out := map[int]map[string]bool{}
	for _, g := range groups {
		for _, r := range g {
			if !pending(r.TrackedDownloadState) {
				continue
			}
			id := r.MovieID
			if kind == arr.Sonarr {
				id = r.EpisodeID
			}
			if id == 0 {
				continue
			}
			if out[id] == nil {
				out[id] = map[string]bool{}
			}
			out[id][r.DownloadID] = true
		}
	}
	return out
}

func othersClaim(claims map[int]map[string]bool, id int, self string) bool {
	for d := range claims[id] {
		if d != self {
			return true
		}
	}
	return false
}

// planned is a file to reprocess and import.
type planned struct {
	file *File
	from arr.ImportItem
	req  arr.Reprocess
}

func (e *Engine) item(ctx context.Context, kind arr.Kind, api API, g []arr.QueueRecord, claims map[int]map[string]bool) *Item {
	first := g[0]
	ctx, span := e.tracer().Start(ctx, "unstuckarr.item", trace.WithAttributes(
		attribute.String("unstuckarr.app", string(kind)), attribute.String("unstuckarr.download_id", first.DownloadID)))
	defer span.End()
	it := &Item{App: kind, DownloadID: first.DownloadID, Title: first.Title, State: first.TrackedDownloadState}
	key := string(kind) + "/" + first.DownloadID
	if !e.lock(key) {
		it.Decision, it.Reasons = Waiting, []string{"an import of this download is under way"}
		return it
	}
	started := false
	defer func() {
		if !started {
			e.unlock(key)
		}
	}()

	if blocking := blockingMessages(g); len(blocking) > 0 {
		it.Decision, it.Reasons = LeftAlone, blocking
		return it
	}
	var plan []planned
	var err error
	if kind == arr.Radarr {
		plan, err = e.planMovie(ctx, api, g, it, claims)
	} else {
		plan, err = e.planEpisodes(ctx, api, g, it, claims)
	}
	if err != nil {
		it.Decision, it.Reasons = Failed, append(it.Reasons, err.Error())
		span.SetStatus(codes.Error, err.Error())
		return it
	}
	if it.Decision != "" {
		return it
	}
	files, err := reprocess(ctx, api, kind, plan, g)
	if err != nil {
		it.Decision, it.Reasons = Failed, append(it.Reasons, err.Error())
		span.SetStatus(codes.Error, err.Error())
		return it
	}
	if len(files) == 0 {
		it.Decision, it.Reasons = LeftAlone, append(it.Reasons, "the application rejects every file on reprocessing")
		return it
	}
	it.Decision = Import
	if !e.Apply {
		it.Import = &Outcome{Status: DryRun}
		return it
	}
	cmd, err := api.StartManualImport(ctx, files)
	if err != nil {
		it.Decision, it.Reasons = Failed, append(it.Reasons, "start manual import: "+err.Error())
		span.SetStatus(codes.Error, err.Error())
		return it
	}
	it.Import = &Outcome{CommandID: cmd.ID, Status: cmd.Status, Result: cmd.Result, Message: cmd.Message}
	e.mu.Lock()
	e.imports = append(e.imports, it)
	if len(e.imports) > keptImports {
		e.imports = slices.Delete(e.imports, 0, len(e.imports)-keptImports)
	}
	e.mu.Unlock()
	started = true
	e.pollers.Add(1)
	go e.poll(context.WithoutCancel(ctx), ctx.Done(), api, key, it)
	return it
}

// poll checks on the item's import until it finishes or CommandTimeout
// passes, then frees the download for another decision.
func (e *Engine) poll(ctx context.Context, stop <-chan struct{}, api API, key string, it *Item) {
	defer e.pollers.Done()
	defer e.unlock(key)
	deadline := time.Now().Add(e.CommandTimeout)
	t := time.NewTicker(e.CommandPoll)
	defer t.Stop()
	for {
		select {
		case <-stop:
			return
		case <-t.C:
		}
		cmd, err := api.Command(ctx, it.Import.CommandID)
		if err != nil {
			e.Log.WarnContext(ctx, "command", "app", it.App, "downloadId", it.DownloadID, "commandId", it.Import.CommandID, "err", err)
		} else {
			e.mu.Lock()
			it.Import.Status, it.Import.Result, it.Import.Message = cmd.Status, cmd.Result, cmd.Message
			e.mu.Unlock()
		}
		if err == nil && cmd.Finished() || time.Now().After(deadline) {
			status := cmd.Status
			if err != nil || !cmd.Finished() {
				status = Abandoned
				e.mu.Lock()
				it.Import.Status = status
				e.mu.Unlock()
			}
			e.outcomes.Add(ctx, 1, metric.WithAttributes(attribute.String("unstuckarr.app", string(it.App)), attribute.String("unstuckarr.status", status)))
			e.Log.InfoContext(ctx, "import finished", "app", it.App, "downloadId", it.DownloadID, "title", it.Title,
				"commandId", it.Import.CommandID, "status", status, "result", cmd.Result, "message", cmd.Message)
			return
		}
	}
}

// blockingMessages returns the reasons in the records' status messages a
// manual import does not overrule.
func blockingMessages(g []arr.QueueRecord) []string {
	var out []string
	for _, r := range g {
		for _, m := range r.StatusMessages {
			reasons := m.Messages
			if len(reasons) == 0 {
				if decide.Header(m.Title) {
					continue
				}
				reasons = []string{m.Title}
			}
			for _, reason := range reasons {
				if !decide.Overrulable(reason) && !slices.Contains(out, reason) {
					out = append(out, reason)
				}
			}
		}
	}
	return out
}

func reasonsOf(rs []arr.Rejection) (blocking []string, any bool) {
	for _, r := range rs {
		if !decide.Overrulable(r.Reason) {
			blocking = append(blocking, r.Reason)
		}
	}
	return blocking, len(rs) > 0
}

// candidates fills it.Files from the listing and returns the indices of the
// files worth importing. A disc structure leaves the download alone.
func (e *Engine) candidates(items []arr.ImportItem, it *Item) []int {
	disc := false
	var out []int
	for i, gi := range items {
		f := File{Path: relPath(gi), Size: gi.Size}
		switch {
		case decide.Disc(f.Path):
			disc = true
			f.Skipped = "part of a disc structure or a disc image"
		default:
			f.Skipped = decide.Skip(f.Path, f.Size, e.MinSize)
		}
		if f.Skipped == "" {
			out = append(out, i)
		}
		it.Files = append(it.Files, f)
	}
	switch {
	case disc:
		it.Decision, it.Reasons = LeftAlone, []string{"a disc structure or image: not imported"}
		return nil
	case len(out) == 0:
		it.Decision, it.Reasons = LeftAlone, []string{"no video file worth importing"}
		return nil
	}
	return out
}

func relPath(gi arr.ImportItem) string {
	if gi.RelativePath != "" {
		return gi.RelativePath
	}
	return gi.Path
}

func (e *Engine) planMovie(ctx context.Context, api API, g []arr.QueueRecord, it *Item, claims map[int]map[string]bool) ([]planned, error) {
	first := g[0]
	movie := first.MovieID
	if movie == 0 {
		it.Decision, it.Reasons = LeftAlone, []string{"the grab names no movie"}
		return nil, nil
	}
	if othersClaim(claims, movie, first.DownloadID) {
		it.Decision, it.Reasons = LeftAlone, []string{fmt.Sprintf("another download of movie %d is awaiting import", movie)}
		return nil, nil
	}
	items, err := api.ManualImport(ctx, first.DownloadID)
	if err != nil {
		return nil, fmt.Errorf("list files: %w", err)
	}
	cands := e.candidates(items, it)
	if it.Decision != "" {
		return nil, nil
	}
	sizes := make([]int64, len(cands))
	for k, i := range cands {
		sizes[k] = items[i].Size
	}
	k, ok := decide.Feature(sizes)
	source := "largest"
	if len(cands) == 1 {
		source = "only"
	}
	if !ok {
		if k, ok = e.askFeature(ctx, items, cands, first, it); !ok {
			it.Decision = LeftAlone
			it.Reasons = append(it.Reasons, "several video files and none stands out as the feature")
			return nil, nil
		}
		source = decide.FromLLM
	}
	for kk, i := range cands {
		if kk != k {
			it.Files[i].Skipped = "not the feature"
		}
	}
	i := cands[k]
	gi, f := items[i], &it.Files[i]
	blocking, rejected := reasonsOf(gi.Rejections)
	switch {
	case !rejected:
		f.Skipped = "the application imports it itself"
		it.Decision, it.Reasons = Waiting, []string{"the feature has no rejection: the application imports it itself"}
		return nil, nil
	case len(blocking) > 0:
		f.Skipped = strings.Join(blocking, "; ")
		it.Decision, it.Reasons = LeftAlone, blocking
		return nil, nil
	case gi.Movie != nil && gi.Movie.ID != movie:
		f.Skipped = fmt.Sprintf("the application matched it to movie %d", gi.Movie.ID)
		it.Decision, it.Reasons = LeftAlone, []string{f.Skipped + ", not the grab's"}
		return nil, nil
	}
	f.MovieID, f.Source = movie, source
	return []planned{{file: f, from: gi, req: arr.Reprocess{
		ID:           gi.ID,
		Path:         gi.Path,
		MovieID:      movie,
		Quality:      quality(gi.Quality, first.Quality),
		Languages:    languages(gi.Languages, first.Languages),
		ReleaseGroup: gi.ReleaseGroup,
		DownloadID:   first.DownloadID,
		IndexerFlags: gi.IndexerFlags,
	}}}, nil
}

func (e *Engine) planEpisodes(ctx context.Context, api API, g []arr.QueueRecord, it *Item, claims map[int]map[string]bool) ([]planned, error) {
	first := g[0]
	series := first.SeriesID
	if series == 0 {
		it.Decision, it.Reasons = LeftAlone, []string{"the grab names no series"}
		return nil, nil
	}
	var allowed []arr.Episode
	var contested []string
	for _, r := range g {
		if r.EpisodeID == 0 || slices.ContainsFunc(allowed, func(e arr.Episode) bool { return e.ID == r.EpisodeID }) {
			continue
		}
		ep := arr.Episode{ID: r.EpisodeID, SeasonNumber: r.SeasonNumber}
		if r.Episode != nil {
			ep = *r.Episode
		}
		if othersClaim(claims, r.EpisodeID, first.DownloadID) {
			contested = append(contested, fmt.Sprintf("S%02dE%02d", ep.SeasonNumber, ep.EpisodeNumber))
			continue
		}
		allowed = append(allowed, ep)
	}
	if len(contested) > 0 {
		it.Reasons = append(it.Reasons, "awaiting import from another download too: "+strings.Join(contested, ", "))
	}
	if len(allowed) == 0 {
		it.Decision = LeftAlone
		it.Reasons = append(it.Reasons, "no grabbed episode to import")
		return nil, nil
	}
	items, err := api.ManualImport(ctx, first.DownloadID)
	if err != nil {
		return nil, fmt.Errorf("list files: %w", err)
	}
	cands := e.candidates(items, it)
	if it.Decision != "" {
		return nil, nil
	}
	var todo []int
	for _, i := range cands {
		blocking, rejected := reasonsOf(items[i].Rejections)
		switch {
		case !rejected && first.TrackedDownloadStatus == "ok":
			it.Decision = Waiting
			it.Reasons = append(it.Reasons, "a file has no rejection and the application has not reported a failure: it imports the download itself")
			return nil, nil
		case !rejected:
			it.Files[i].Skipped = "no rejection: left to the application"
		case len(blocking) > 0:
			it.Files[i].Skipped = strings.Join(blocking, "; ")
		default:
			todo = append(todo, i)
		}
	}
	efs := make([]decide.EpisodeFile, len(todo))
	for k, i := range todo {
		efs[k] = decide.EpisodeFile{Path: it.Files[i].Path}
		for _, ep := range items[i].Episodes {
			efs[k].Proposed = append(efs[k].Proposed, ep.ID)
		}
	}
	matches := decide.Episodes(efs, allowed)
	e.askEpisodes(ctx, items, todo, matches, allowed, first, it)

	var plan []planned
	for k, i := range todo {
		m, f, gi := matches[k], &it.Files[i], items[i]
		if len(m.EpisodeIDs) == 0 {
			f.Skipped = m.Reason
			if f.Skipped == "" {
				f.Skipped = "no grabbed episode matches it"
			}
			continue
		}
		season := -1
		for _, ep := range allowed {
			if slices.Contains(m.EpisodeIDs, ep.ID) {
				if season >= 0 && ep.SeasonNumber != season {
					season = -2
					break
				}
				season = ep.SeasonNumber
			}
		}
		if season < 0 {
			f.Skipped = "its episodes are of several seasons"
			continue
		}
		f.EpisodeIDs, f.Source = m.EpisodeIDs, m.Source
		releaseType := gi.ReleaseType
		if releaseType == "" || releaseType == "unknown" {
			switch {
			case len(g) > 1:
				releaseType = "seasonPack"
			case len(m.EpisodeIDs) > 1:
				releaseType = "multiEpisode"
			default:
				releaseType = "singleEpisode"
			}
		}
		plan = append(plan, planned{file: f, from: gi, req: arr.Reprocess{
			ID:           gi.ID,
			Path:         gi.Path,
			SeriesID:     series,
			SeasonNumber: &season,
			EpisodeIDs:   m.EpisodeIDs,
			Quality:      quality(gi.Quality, first.Quality),
			Languages:    languages(gi.Languages, first.Languages),
			ReleaseGroup: gi.ReleaseGroup,
			DownloadID:   first.DownloadID,
			IndexerFlags: gi.IndexerFlags,
			ReleaseType:  releaseType,
		}})
	}
	if len(plan) == 0 {
		it.Decision = LeftAlone
		it.Reasons = append(it.Reasons, "no file is matched to a grabbed episode")
	}
	return plan, nil
}

// quality is the file's quality when the application knows it, else the
// grab's.
func quality(file, grab json.RawMessage) json.RawMessage {
	switch {
	case arr.KnownQuality(file):
		return file
	case arr.KnownQuality(grab):
		return grab
	}
	return arr.UnknownQuality
}

// languages is the file's languages when the application knows them, else
// the grab's.
func languages(file, grab json.RawMessage) json.RawMessage {
	switch {
	case arr.KnownLanguages(file):
		return file
	case arr.KnownLanguages(grab):
		return grab
	}
	return json.RawMessage(`[]`)
}

// reprocess has the application evaluate the planned files with their
// targets and returns those it raises no blocking rejection against, as
// files of a ManualImport command. A file the application cannot reprocess
// because it can parse neither its name nor the download's is judged by
// unreprocessed instead.
func reprocess(ctx context.Context, api API, kind arr.Kind, plan []planned, g []arr.QueueRecord) ([]arr.ImportFile, error) {
	reqs := make([]arr.Reprocess, len(plan))
	for i, p := range plan {
		reqs[i] = p.req
	}
	res, err := api.Reprocess(ctx, reqs)
	unparsed := make([]bool, len(plan))
	if err != nil && cannotParse(err) {
		res = make([]arr.ImportItem, len(plan))
		for i, req := range reqs {
			one, err := api.Reprocess(ctx, []arr.Reprocess{req})
			switch {
			case err != nil && cannotParse(err):
				unparsed[i] = true
			case err != nil:
				return nil, fmt.Errorf("reprocess: %w", err)
			case len(one) != 1:
				return nil, fmt.Errorf("reprocess: %d files back for 1 sent", len(one))
			default:
				res[i] = one[0]
			}
		}
	} else if err != nil {
		return nil, fmt.Errorf("reprocess: %w", err)
	}
	if len(res) != len(reqs) {
		return nil, fmt.Errorf("reprocess: %d files back for %d sent", len(res), len(reqs))
	}
	var files []arr.ImportFile
	for i, r := range res {
		p := plan[i]
		if unparsed[i] {
			f, skip, err := unreprocessed(ctx, api, kind, p, g)
			if err != nil {
				return nil, err
			}
			if skip != "" {
				p.file.Skipped = skip
				continue
			}
			p.file.Unreprocessed = true
			files = append(files, f)
			continue
		}
		blocking, _ := reasonsOf(r.Rejections)
		if kind == arr.Radarr && (r.Movie == nil || r.Movie.ID != p.req.MovieID) {
			blocking = append(blocking, "reprocessing matched it to another movie")
		}
		if kind == arr.Sonarr {
			var ids []int
			for _, ep := range r.Episodes {
				ids = append(ids, ep.ID)
			}
			slices.Sort(ids)
			if !slices.Equal(ids, p.req.EpisodeIDs) {
				blocking = append(blocking, "reprocessing matched it to other episodes")
			}
		}
		if len(blocking) > 0 {
			p.file.Skipped = "rejected on reprocessing: " + strings.Join(blocking, "; ")
			continue
		}
		f := arr.ImportFile{
			Path:         p.req.Path,
			FolderName:   p.from.FolderName,
			MovieID:      p.req.MovieID,
			SeriesID:     p.req.SeriesID,
			EpisodeIDs:   p.req.EpisodeIDs,
			Quality:      quality(r.Quality, p.req.Quality),
			Languages:    languages(r.Languages, p.req.Languages),
			ReleaseGroup: cmp(r.ReleaseGroup, p.req.ReleaseGroup),
			IndexerFlags: r.IndexerFlags,
			ReleaseType:  cmp(r.ReleaseType, p.req.ReleaseType),
			DownloadID:   p.req.DownloadID,
		}
		if kind == arr.Radarr {
			f.ReleaseType = ""
		}
		files = append(files, f)
	}
	return files, nil
}

// cannotParse reports whether err is the application failing to reprocess
// a file because it can parse neither the file's path nor the download's
// title: Radarr and Sonarr answer 500 with "Unable to parse movie (or
// episode) info from path".
func cannotParse(err error) bool {
	var e *arr.Error
	return errors.As(err, &e) && e.Status == http.StatusInternalServerError && strings.Contains(e.Body, "info from path")
}

// unreprocessed returns the ManualImport file for a planned file the
// application could not reprocess, or why it is not imported. The listing's
// rejections were all overrulable, but the checks reprocessing runs, such as
// whether it is an upgrade, never ran; so it is imported only into a movie
// or episodes without a file, and with the quality the grab was parsed as.
func unreprocessed(ctx context.Context, api API, kind arr.Kind, p planned, g []arr.QueueRecord) (arr.ImportFile, string, error) {
	if !arr.KnownQuality(p.req.Quality) {
		return arr.ImportFile{}, "the application cannot reprocess it and its quality is unknown", nil
	}
	if kind == arr.Radarr {
		m, err := api.Movie(ctx, p.req.MovieID)
		if err != nil {
			return arr.ImportFile{}, "", fmt.Errorf("movie %d: %w", p.req.MovieID, err)
		}
		if m.HasFile {
			return arr.ImportFile{}, "the application cannot reprocess it and the movie has a file: whether it is an upgrade is unchecked", nil
		}
	} else {
		for _, r := range g {
			if r.EpisodeHasFile && slices.Contains(p.req.EpisodeIDs, r.EpisodeID) {
				return arr.ImportFile{}, "the application cannot reprocess it and an episode has a file: whether it is an upgrade is unchecked", nil
			}
		}
	}
	return arr.ImportFile{
		Path:         p.req.Path,
		FolderName:   p.from.FolderName,
		MovieID:      p.req.MovieID,
		SeriesID:     p.req.SeriesID,
		EpisodeIDs:   p.req.EpisodeIDs,
		Quality:      p.req.Quality,
		Languages:    p.req.Languages,
		ReleaseGroup: p.req.ReleaseGroup,
		IndexerFlags: p.req.IndexerFlags,
		ReleaseType:  p.req.ReleaseType,
		DownloadID:   p.req.DownloadID,
	}, "", nil
}

func cmp(v, fallback string) string {
	if v == "" {
		return fallback
	}
	return v
}

func mib(n int64) string { return fmt.Sprintf("%d MiB", n>>20) }

// askFeature asks the LLM which candidate is the movie's feature and
// returns its index among cands.
func (e *Engine) askFeature(ctx context.Context, items []arr.ImportItem, cands []int, first arr.QueueRecord, it *Item) (int, bool) {
	if e.LLM == nil {
		return 0, false
	}
	movie := ""
	if m := items[cands[0]].Movie; m != nil {
		movie = fmt.Sprintf("%s (%d)", m.Title, m.Year)
	}
	files := make([]string, len(cands))
	opts := map[string]string{"none": "None of the files is the whole feature, or more than one is"}
	for k, i := range cands {
		files[k] = it.Files[i].Path + " (" + mib(items[i].Size) + ")"
		opts[fmt.Sprintf("file_%d", k)] = files[k]
	}
	ans, err := e.LLM.Ask(ctx, map[string]any{"release": first.Title, "movie": movie, "files": files}, map[string]typesafe.Question{
		"feature": {
			Instructions: "Which of the `files` of the `release` is the feature film `movie` itself, not an extra, a sample, a trailer or one part of it?",
			Options:      opts,
		},
	})
	if err != nil {
		it.Reasons = append(it.Reasons, "LLM: "+err.Error())
		return 0, false
	}
	a := ans["feature"]
	if a.Choice == "none" || a.Confidence < e.MinConfidence {
		it.Reasons = append(it.Reasons, fmt.Sprintf("LLM: no feature (%s, confidence %.2f)", a.Choice, a.Confidence))
		return 0, false
	}
	var k int
	if _, err := fmt.Sscanf(a.Choice, "file_%d", &k); err != nil || k < 0 || k >= len(cands) {
		return 0, false
	}
	return k, true
}

// maxOptions is the most options a choice question takes, less skip.
const maxOptions = 254

// askEpisodes asks the LLM which of the allowed episodes not matched yet
// each unresolved file is, and fills in the matches it answers.
func (e *Engine) askEpisodes(ctx context.Context, items []arr.ImportItem, todo []int, matches []decide.Match, allowed []arr.Episode, first arr.QueueRecord, it *Item) {
	var unresolved []int
	taken := map[int]bool{}
	for k, m := range matches {
		for _, id := range m.EpisodeIDs {
			taken[id] = true
		}
		if len(m.EpisodeIDs) == 0 && m.Reason == "" {
			unresolved = append(unresolved, k)
		}
	}
	if len(unresolved) == 0 || e.LLM == nil {
		return
	}
	opts := map[string]string{"skip": "None of the `episodes`: an extra, a sample, a recap, or an episode not listed"}
	var episodes []string
	for _, ep := range allowed {
		if taken[ep.ID] {
			continue
		}
		d := fmt.Sprintf("S%02dE%02d %q", ep.SeasonNumber, ep.EpisodeNumber, ep.Title)
		if ep.AbsoluteEpisodeNumber > 0 {
			d += fmt.Sprintf(", absolute %d", ep.AbsoluteEpisodeNumber)
		}
		if ep.AirDate != "" {
			d += ", aired " + ep.AirDate
		}
		opts[fmt.Sprintf("ep_%d", ep.ID)] = d
		episodes = append(episodes, d)
	}
	if len(episodes) == 0 {
		return
	}
	if len(episodes) > maxOptions {
		it.Reasons = append(it.Reasons, "LLM: too many episodes to ask about")
		return
	}
	series := ""
	if s := items[todo[0]].Series; s != nil {
		series = s.Title
	}
	files := make([]string, len(todo))
	for k, i := range todo {
		files[k] = it.Files[i].Path + " (" + mib(items[i].Size) + ")"
	}
	qs := map[string]typesafe.Question{}
	for _, k := range unresolved {
		qs[fmt.Sprintf("file_%d", k)] = typesafe.Question{
			Instructions: fmt.Sprintf("Which of the `episodes` of `series` is the video file %q among the `files` of the `release`?", it.Files[todo[k]].Path),
			Options:      opts,
		}
	}
	ans, err := e.LLM.Ask(ctx, map[string]any{"release": first.Title, "series": series, "files": files, "episodes": episodes}, qs)
	if err != nil {
		it.Reasons = append(it.Reasons, "LLM: "+err.Error())
		return
	}
	for _, k := range unresolved {
		a := ans[fmt.Sprintf("file_%d", k)]
		var id int
		if a.Choice == "skip" || a.Confidence < e.MinConfidence {
			continue
		}
		if _, err := fmt.Sscanf(a.Choice, "ep_%d", &id); err != nil || taken[id] {
			continue
		}
		matches[k] = decide.Match{EpisodeIDs: []int{id}, Source: decide.FromLLM}
	}
	decide.Unclaim(matches)
}
