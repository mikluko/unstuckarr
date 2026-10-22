package unstuck

import (
	"context"
	"encoding/json"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/mikluko/unstuckarr/internal/arr"
	"github.com/mikluko/unstuckarr/internal/typesafe"
)

// fakeArr serves the queue, the manual import listing and reprocessing, and
// commands, as Radarr or Sonarr does.
type fakeArr struct {
	kind    arr.Kind
	queue   []arr.QueueRecord
	listing map[string][]arr.ImportItem
	// rejections are put on a reprocessed file of the path.
	rejections map[string][]arr.Rejection
	// broken are downloads whose listing fails.
	broken map[string]bool

	mu          sync.Mutex
	listed      map[string]int
	reprocessed [][]arr.Reprocess
	commands    []command
	polls       atomic.Int64
}

type command struct {
	Name       string           `json:"name"`
	ImportMode string           `json:"importMode"`
	Files      []arr.ImportFile `json:"files"`
}

func (f *fakeArr) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if r.Header.Get("X-Api-Key") != "key" {
		http.Error(w, "unauthorized", http.StatusUnauthorized)
		return
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	var out any
	switch r.Method + " " + r.URL.Path {
	case "GET /api/v3/queue":
		records := f.queue
		if r.URL.Query().Get("page") != "1" {
			records = nil
		}
		if f.kind == arr.Sonarr && r.URL.Query().Get("includeEpisode") != "true" {
			http.Error(w, "episodes not asked for", http.StatusBadRequest)
			return
		}
		out = map[string]any{"page": 1, "pageSize": 200, "totalRecords": len(f.queue), "records": records}
	case "GET /api/v3/manualimport":
		dl := r.URL.Query().Get("downloadId")
		if r.URL.Query().Has("seriesId") || r.URL.Query().Has("movieId") {
			http.Error(w, "Sonarr lists another download's files when given a seriesId", http.StatusBadRequest)
			return
		}
		if f.listed == nil {
			f.listed = map[string]int{}
		}
		f.listed[dl]++
		if f.broken[dl] {
			http.Error(w, "boom", http.StatusInternalServerError)
			return
		}
		out = f.listing[dl]
	case "POST /api/v3/manualimport":
		var reqs []arr.Reprocess
		if err := json.NewDecoder(r.Body).Decode(&reqs); err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		f.reprocessed = append(f.reprocessed, reqs)
		var items []arr.ImportItem
		for _, q := range reqs {
			it := arr.ImportItem{ID: q.ID, Path: q.Path, SeasonNumber: q.SeasonNumber, Quality: q.Quality, Languages: q.Languages,
				ReleaseGroup: q.ReleaseGroup, DownloadID: q.DownloadID, IndexerFlags: q.IndexerFlags, ReleaseType: q.ReleaseType,
				Rejections: f.rejections[q.Path]}
			if q.MovieID != 0 {
				it.Movie = &arr.Ref{ID: q.MovieID}
			}
			for _, id := range q.EpisodeIDs {
				it.Episodes = append(it.Episodes, arr.Episode{ID: id})
			}
			items = append(items, it)
		}
		out = items
	case "POST /api/v3/command":
		var c command
		if err := json.NewDecoder(r.Body).Decode(&c); err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		f.commands = append(f.commands, c)
		out = arr.Command{ID: 42, Name: c.Name, Status: "queued"}
	case "GET /api/v3/command/42":
		out = arr.Command{ID: 42, Name: "ManualImport", Status: "started"}
		if f.polls.Add(1) > 1 {
			out = arr.Command{ID: 42, Name: "ManualImport", Status: "completed", Result: "successful", Message: "Completed"}
		}
	default:
		http.NotFound(w, r)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(out)
}

func (f *fakeArr) client(t *testing.T) *arr.Client {
	srv := httptest.NewServer(f)
	t.Cleanup(srv.Close)
	return &arr.Client{Kind: f.kind, URL: srv.URL, Key: "key", HTTP: srv.Client()}
}

// fakeTypeSafe answers each file question with answer(file path).
type fakeTypeSafe struct {
	answer   func(path string) string
	requests atomic.Int64
	// options are the options of the last question asked.
	options []string
}

func (f *fakeTypeSafe) client(t *testing.T, budget *typesafe.Budget) *typesafe.Client {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		f.requests.Add(1)
		var req struct {
			Questions map[string]struct {
				Instructions string            `json:"instructions"`
				Criteria     map[string]string `json:"criteria"`
			} `json:"questions"`
		}
		if r.Header.Get("Authorization") != "Bearer key" || json.NewDecoder(r.Body).Decode(&req) != nil {
			http.Error(w, "bad request", http.StatusBadRequest)
			return
		}
		answers := map[string]any{}
		for id, q := range req.Questions {
			f.options = f.options[:0]
			for o := range q.Criteria {
				f.options = append(f.options, o)
			}
			slices.Sort(f.options)
			start := strings.Index(q.Instructions, `"`)
			end := strings.Index(q.Instructions[start+1:], `"`)
			answers[id] = map[string]any{"type": "choice", "choice": f.answer(q.Instructions[start+1 : start+1+end]), "confidence": 0.9}
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"answers": answers, "usage": map[string]int{"input_tokens": 1000, "output_tokens": 50}})
	}))
	t.Cleanup(srv.Close)
	c := typesafe.New("key", budget)
	c.URL, c.Backoff = srv.URL, time.Millisecond
	return c
}

func engine(apply bool) *Engine {
	return &Engine{Apply: apply, MinSize: 50e6, MinConfidence: 0.5, CommandPoll: time.Millisecond, CommandTimeout: time.Minute,
		Log: slog.New(slog.DiscardHandler)}
}

var (
	bluray   = json.RawMessage(`{"quality":{"id":7,"name":"Bluray-1080p"},"revision":{"version":1,"real":0,"isRepack":false}}`)
	japanese = json.RawMessage(`[{"id":8,"name":"Japanese"}]`)
)

func movieRecord(dl string, movie int, title string) arr.QueueRecord {
	return arr.QueueRecord{ID: movie * 10, MovieID: movie, Title: title, Status: "completed", TrackedDownloadStatus: "ok",
		TrackedDownloadState: "importPending", DownloadID: dl, Quality: bluray, Languages: japanese}
}

func unparsable(path string, size int64, movie int) arr.ImportItem {
	rel := path[strings.LastIndex(path, "/")+1:]
	if i := strings.Index(path, "/dl/"); i >= 0 {
		rel = strings.SplitN(path[i+len("/dl/"):], "/", 2)[1]
	}
	it := arr.ImportItem{ID: int(size % 1000), Path: path, RelativePath: rel, Size: size, Languages: json.RawMessage(`[]`),
		Rejections: []arr.Rejection{{Reason: "Unable to parse file", Type: "permanent"}}}
	if movie != 0 {
		it.Movie = &arr.Ref{ID: movie, Title: "Kiki's Delivery Service", Year: 1989}
	}
	return it
}

func TestRadarrUnparsable(t *testing.T) {
	f := &fakeArr{kind: arr.Radarr,
		queue:   []arr.QueueRecord{movieRecord("KIKI", 3, "Kiki_Delivery_Service.mkv")},
		listing: map[string][]arr.ImportItem{"KIKI": {unparsable("/mnt/radarr/Kiki_Delivery_Service.mkv", 35e9, 3)}},
	}
	e := engine(true)
	run := e.Run(context.Background(), arr.Radarr, f.client(t))
	e.Wait()
	if run.Error != "" || len(run.Items) != 1 {
		t.Fatalf("run %+v", run)
	}
	it := run.Items[0]
	if it.Decision != Import || it.Files[0].MovieID != 3 || it.Files[0].Source != "only" {
		t.Fatalf("item %+v", it)
	}
	if it.Import.Status != "completed" || it.Import.Result != "successful" {
		t.Fatalf("import %+v", it.Import)
	}
	if len(f.commands) != 1 {
		t.Fatalf("%d commands", len(f.commands))
	}
	c := f.commands[0]
	want := arr.ImportFile{Path: "/mnt/radarr/Kiki_Delivery_Service.mkv", MovieID: 3, Quality: bluray, Languages: japanese, DownloadID: "KIKI"}
	if c.Name != "ManualImport" || c.ImportMode != "auto" || len(c.Files) != 1 || !sameFile(c.Files[0], want) {
		t.Fatalf("command %+v", c)
	}
	if r := f.reprocessed[0][0]; r.MovieID != 3 || !arr.KnownQuality(r.Quality) {
		t.Fatalf("reprocessed %+v", r)
	}
	b, err := e.StatusJSON()
	if err != nil || !strings.Contains(string(b), `"commandId":42`) || !strings.Contains(string(b), `"apply":true`) {
		t.Fatalf("status %s, %v", b, err)
	}
}

func sameFile(a, b arr.ImportFile) bool {
	ja, _ := json.Marshal(a)
	jb, _ := json.Marshal(b)
	var ma, mb any
	_ = json.Unmarshal(ja, &ma)
	_ = json.Unmarshal(jb, &mb)
	ja, _ = json.Marshal(ma)
	jb, _ = json.Marshal(mb)
	return string(ja) == string(jb)
}

func TestDryRun(t *testing.T) {
	f := &fakeArr{kind: arr.Radarr,
		queue:   []arr.QueueRecord{movieRecord("KIKI", 3, "Kiki_Delivery_Service.mkv")},
		listing: map[string][]arr.ImportItem{"KIKI": {unparsable("/mnt/radarr/Kiki_Delivery_Service.mkv", 35e9, 3)}},
	}
	e := engine(false)
	run := e.Run(context.Background(), arr.Radarr, f.client(t))
	e.Wait()
	if it := run.Items[0]; it.Decision != Import || it.Import.Status != DryRun {
		t.Fatalf("item %+v", it)
	}
	if len(f.commands) != 0 || f.polls.Load() != 0 {
		t.Fatal("a dry run starts no command")
	}
}

func TestRadarrLeftAlone(t *testing.T) {
	warned := movieRecord("HERON", 8, "The.Boy.and.the.Heron.2023")
	warned.TrackedDownloadStatus = "warning"
	warned.StatusMessages = []arr.StatusMessage{
		{Title: "One or more movies expected in this release were not imported or missing"},
		{Title: "The.Boy.and.the.Heron.2023.mkv", Messages: []string{"Not an upgrade for existing movie file(s)"}},
	}
	parsed := unparsable("/mnt/radarr/dl/Heron/The.Boy.and.the.Heron.2023.2160p.mkv", 24e9, 8)
	parsed.Rejections = nil
	for _, c := range []struct {
		name       string
		queue      []arr.QueueRecord
		listing    []arr.ImportItem
		rejections map[string][]arr.Rejection
		decision   string
		reason     string
		reprocess  bool
	}{
		{name: "queue warning", queue: []arr.QueueRecord{warned}, decision: LeftAlone, reason: "Not an upgrade"},
		{name: "reprocess rejection", queue: []arr.QueueRecord{movieRecord("KIKI", 3, "Kiki")},
			listing:    []arr.ImportItem{unparsable("/mnt/radarr/Kiki.mkv", 35e9, 3)},
			rejections: map[string][]arr.Rejection{"/mnt/radarr/Kiki.mkv": {{Reason: "Not an upgrade for existing movie file(s)", Type: "permanent"}}},
			decision:   LeftAlone, reason: "rejects every file", reprocess: true},
		{name: "disc", queue: []arr.QueueRecord{movieRecord("KIKI", 3, "Kiki BDMV")},
			listing:  []arr.ImportItem{unparsable("/mnt/radarr/dl/Kiki/BDMV/STREAM/00001.m2ts", 30e9, 3)},
			decision: LeftAlone, reason: "disc structure"},
		{name: "parsed", queue: []arr.QueueRecord{movieRecord("HERON", 8, "Heron")}, listing: []arr.ImportItem{parsed},
			decision: Waiting, reason: "imports it itself"},
		{name: "two downloads of a movie", queue: []arr.QueueRecord{movieRecord("KIMI", 8, "Kimitachi"), movieRecord("HERON", 8, "Heron")},
			listing:  []arr.ImportItem{unparsable("/mnt/radarr/Kimitachi.mkv", 24e9, 8)},
			decision: LeftAlone, reason: "another download of movie 8"},
		{name: "no feature", queue: []arr.QueueRecord{movieRecord("KIKI", 3, "Kiki")},
			listing:  []arr.ImportItem{unparsable("/mnt/radarr/dl/Kiki/a.mkv", 20e9, 3), unparsable("/mnt/radarr/dl/Kiki/b.mkv", 21e9, 3)},
			decision: LeftAlone, reason: "none stands out"},
	} {
		t.Run(c.name, func(t *testing.T) {
			listing := map[string][]arr.ImportItem{}
			for _, r := range c.queue {
				listing[r.DownloadID] = c.listing
			}
			f := &fakeArr{kind: arr.Radarr, queue: c.queue, listing: listing, rejections: c.rejections}
			e := engine(true)
			run := e.Run(context.Background(), arr.Radarr, f.client(t))
			e.Wait()
			it := run.Items[0]
			if it.Decision != c.decision || !strings.Contains(strings.Join(it.Reasons, "; "), c.reason) {
				t.Fatalf("item %+v", it)
			}
			if len(f.commands) != 0 {
				t.Fatal("no import is started")
			}
			if (len(f.reprocessed) > 0) != c.reprocess {
				t.Fatalf("reprocessed %v", f.reprocessed)
			}
		})
	}
}

var grabbed = []arr.Episode{
	{ID: 730, SeasonNumber: 1, EpisodeNumber: 1, Title: "Pilot", AirDate: "2026-08-17"},
	{ID: 731, SeasonNumber: 1, EpisodeNumber: 2, Title: "Trust Fall", AirDate: "2026-08-17"},
	{ID: 732, SeasonNumber: 1, EpisodeNumber: 3, Title: "OutKast", AirDate: "2026-08-24"},
}

func seriesRecords(dl, title string, episodes []arr.Episode) []arr.QueueRecord {
	var out []arr.QueueRecord
	for _, ep := range episodes {
		out = append(out, arr.QueueRecord{ID: ep.ID * 10, SeriesID: 17, EpisodeID: ep.ID, SeasonNumber: ep.SeasonNumber, Episode: &ep,
			Title: title, Status: "completed", TrackedDownloadStatus: "warning", TrackedDownloadState: "importPending", DownloadID: dl,
			Quality: bluray, Languages: japanese,
			StatusMessages: []arr.StatusMessage{
				{Title: "One or more episodes expected in this release were not imported or missing from the release"},
				{Title: "file.mkv", Messages: []string{"Unable to parse file"}},
			}})
	}
	return out
}

func episodeItem(path string, rejection string, proposed ...arr.Episode) arr.ImportItem {
	it := arr.ImportItem{Path: "/mnt/sonarr/dl/" + path, RelativePath: path[strings.Index(path, "/")+1:], FolderName: path[:strings.Index(path, "/")],
		Size: 2e9, Series: &arr.Ref{ID: 17, Title: "Lanterns"}, Episodes: proposed, Languages: json.RawMessage(`[]`),
		Rejections: []arr.Rejection{{Reason: rejection, Type: "permanent"}}}
	return it
}

func episodeIDs(c command) [][]int {
	var out [][]int
	for _, f := range c.Files {
		out = append(out, f.EpisodeIDs)
	}
	return out
}

func TestSonarrGroupedByDownload(t *testing.T) {
	f := &fakeArr{kind: arr.Sonarr,
		queue: seriesRecords("PACK", "Lanterns.S01.1080p", grabbed),
		listing: map[string][]arr.ImportItem{"PACK": {
			episodeItem("Lanterns.S01/Lanterns.S01E01.mkv", "Episode has a TBA title and recently aired", grabbed[0]),
			episodeItem("Lanterns.S01/Lanterns.S01E02.mkv", "Episode has a TBA title and recently aired", grabbed[1]),
			episodeItem("Lanterns.S01/Lanterns.S01E03.mkv", "Episode has a TBA title and recently aired", grabbed[2]),
		}},
	}
	e := engine(true)
	run := e.Run(context.Background(), arr.Sonarr, f.client(t))
	e.Wait()
	if len(run.Items) != 1 || f.listed["PACK"] != 1 {
		t.Fatalf("%d items, listed %d times: one per download", len(run.Items), f.listed["PACK"])
	}
	if it := run.Items[0]; it.Decision != Import || it.Import.Status != "completed" {
		t.Fatalf("item %+v", it)
	}
	c := f.commands[0]
	if got := episodeIDs(c); !slices.EqualFunc(got, [][]int{{730}, {731}, {732}}, slices.Equal) {
		t.Fatalf("episodes %v", got)
	}
	if fl := c.Files[0]; fl.SeriesID != 17 || fl.FolderName != "Lanterns.S01" || fl.ReleaseType != "seasonPack" || fl.DownloadID != "PACK" {
		t.Fatalf("file %+v", fl)
	}
}

// russianPack is a season pack whose files are named by their Russian
// episode titles alone.
func russianPack() *fakeArr {
	return &fakeArr{kind: arr.Sonarr,
		queue: seriesRecords("RU", "Фонари / Lanterns (сезон 1) WEB-DL 1080p", grabbed),
		listing: map[string][]arr.ImportItem{"RU": {
			episodeItem("Фонари/Изгои.mkv", "Unable to parse file"),
			episodeItem("Фонари/Пилот.mkv", "Unable to parse file"),
			episodeItem("Фонари/Доверие.mkv", "Unable to parse file"),
			episodeItem("Фонари/Extras/Интервью.mkv", "Unable to parse file"),
		}},
	}
}

var titles = map[string]string{"Пилот.mkv": "ep_730", "Доверие.mkv": "ep_731", "Изгои.mkv": "ep_732"}

func TestSonarrLLM(t *testing.T) {
	f := russianPack()
	ts := &fakeTypeSafe{answer: func(p string) string { return titles[p] }}
	e := engine(true)
	e.Budget = &typesafe.Budget{Daily: 10000}
	e.LLM = ts.client(t, e.Budget)
	run := e.Run(context.Background(), arr.Sonarr, f.client(t))
	e.Wait()
	it := run.Items[0]
	if it.Decision != Import || ts.requests.Load() != 1 {
		t.Fatalf("item %+v, %d requests", it, ts.requests.Load())
	}
	if !slices.Equal(ts.options, []string{"ep_730", "ep_731", "ep_732", "skip"}) {
		t.Fatalf("options %v: only the grabbed episodes", ts.options)
	}
	if got := episodeIDs(f.commands[0]); !slices.EqualFunc(got, [][]int{{732}, {730}, {731}}, slices.Equal) {
		t.Fatalf("episodes %v", got)
	}
	if it.Files[0].Source != "llm" || it.Files[3].Skipped != "in an extras folder" {
		t.Fatalf("files %+v", it.Files)
	}
	if u := e.Budget.Usage(); u.Used != 1050 {
		t.Fatalf("budget %+v", u)
	}
}

func TestSonarrLLMOutsideGrab(t *testing.T) {
	f := russianPack()
	ts := &fakeTypeSafe{answer: func(p string) string {
		if p == "Изгои.mkv" {
			return "ep_733"
		}
		return titles[p]
	}}
	e := engine(true)
	e.Budget = &typesafe.Budget{Daily: 10000}
	e.LLM = ts.client(t, e.Budget)
	run := e.Run(context.Background(), arr.Sonarr, f.client(t))
	e.Wait()
	it := run.Items[0]
	if it.Decision != LeftAlone || !strings.Contains(strings.Join(it.Reasons, "; "), "not one of its options") {
		t.Fatalf("item %+v", it)
	}
	if len(f.commands) != 0 || len(f.reprocessed) != 0 {
		t.Fatal("an answer outside the grab imports nothing")
	}
}

func TestSonarrBudgetSpent(t *testing.T) {
	f := russianPack()
	ts := &fakeTypeSafe{answer: func(p string) string { return titles[p] }}
	e := engine(true)
	e.Budget = &typesafe.Budget{Daily: 1000}
	e.Budget.Spend(1000)
	e.LLM = ts.client(t, e.Budget)
	run := e.Run(context.Background(), arr.Sonarr, f.client(t))
	e.Wait()
	it := run.Items[0]
	if it.Decision != LeftAlone || !strings.Contains(strings.Join(it.Reasons, "; "), "budget") || ts.requests.Load() != 0 {
		t.Fatalf("item %+v, %d requests", it, ts.requests.Load())
	}
}

func TestSonarrNumberedRussianPack(t *testing.T) {
	f := &fakeArr{kind: arr.Sonarr,
		queue: seriesRecords("RU", "Фонари (сезон 1)", grabbed),
		listing: map[string][]arr.ImportItem{"RU": {
			episodeItem("Фонари/01. Пилот.mkv", "Unable to parse file"),
			episodeItem("Фонари/02. Доверие.mkv", "Unable to parse file"),
			episodeItem("Фонари/03. Изгои.mkv", "Failed to import episode, Destination already exists."),
		}},
	}
	e := engine(false)
	run := e.Run(context.Background(), arr.Sonarr, f.client(t))
	it := run.Items[0]
	if it.Decision != Import || it.Files[0].Source != "season-number" || !slices.Equal(it.Files[1].EpisodeIDs, []int{731}) {
		t.Fatalf("item %+v", it)
	}
	if !strings.Contains(it.Files[2].Skipped, "Destination already exists") {
		t.Fatalf("a file with a blocking rejection is skipped: %+v", it.Files[2])
	}
}

func TestBusyDownload(t *testing.T) {
	f := &fakeArr{kind: arr.Radarr,
		queue:   []arr.QueueRecord{movieRecord("KIKI", 3, "Kiki")},
		listing: map[string][]arr.ImportItem{"KIKI": {unparsable("/mnt/radarr/Kiki.mkv", 35e9, 3)}},
	}
	e := engine(true)
	if !e.lock("radarr/KIKI") {
		t.Fatal("lock")
	}
	if it := e.Run(context.Background(), arr.Radarr, f.client(t)).Items[0]; it.Decision != Waiting {
		t.Fatalf("item %+v", it)
	}
	if f.listed["KIKI"] != 0 {
		t.Fatal("a download under import is not looked at")
	}
}

func TestErrorIsolation(t *testing.T) {
	f := &fakeArr{kind: arr.Radarr,
		queue:   []arr.QueueRecord{movieRecord("BROKEN", 1, "Broken"), movieRecord("KIKI", 3, "Kiki")},
		listing: map[string][]arr.ImportItem{"KIKI": {unparsable("/mnt/radarr/Kiki.mkv", 35e9, 3)}},
		broken:  map[string]bool{"BROKEN": true},
	}
	e := engine(false)
	run := e.Run(context.Background(), arr.Radarr, f.client(t))
	if run.Items[0].Decision != Failed || !strings.Contains(run.Items[0].Reasons[0], "500") || run.Items[1].Decision != Import {
		t.Fatalf("items %+v %+v", run.Items[0], run.Items[1])
	}
}
