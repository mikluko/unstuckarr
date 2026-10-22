package arr

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
)

// Records and files as the live Radarr and Sonarr return them, trimmed.
const (
	radarrRecord = `{"movieId":3,"languages":[{"id":8,"name":"Japanese"}],"quality":{"quality":{"id":30,"name":"Remux-1080p","source":"bluray","resolution":1080,"modifier":"remux"},"revision":{"version":1,"real":0,"isRepack":false}},"customFormatScore":0,"size":35286380556,"title":"Kiki_Delivery_Service.mkv","added":"2026-10-19T15:39:32Z","status":"completed","trackedDownloadStatus":"ok","trackedDownloadState":"importPending","statusMessages":[],"downloadId":"31B38E6FE9F52E92C3FFF8C777297D8E87E977E0","protocol":"torrent","downloadClient":"qbittorrent","indexer":"RuTracker.org (Prowlarr)","outputPath":"/mnt/seedbox/radarr/Kiki_Delivery_Service.mkv","sizeleft":0,"id":467040178}`
	radarrFile   = `{"path":"/mnt/seedbox/radarr/Kiki_Delivery_Service.mkv","relativePath":"Kiki_Delivery_Service.mkv","name":"Kiki_Delivery_Service","size":35286380556,"qualityWeight":0,"downloadId":"31B38E6FE9F52E92C3FFF8C777297D8E87E977E0","customFormats":[],"customFormatScore":0,"indexerFlags":0,"rejections":[{"reason":"Unable to parse file","type":"permanent"}],"id":215604721,"movie":{"id":3,"title":"Kiki's Delivery Service","year":1989}}`
	sonarrRecord = `{"seriesId":17,"episodeId":737,"seasonNumber":1,"languages":[{"id":1,"name":"English"}],"quality":{"quality":{"id":3,"name":"WEBDL-1080p","source":"web","resolution":1080},"revision":{"version":1,"real":0,"isRepack":false}},"title":"Lanterns (Season 1) WEB-DL 1080p","status":"completed","trackedDownloadStatus":"warning","trackedDownloadState":"importPending","statusMessages":[{"title":"One or more episodes expected in this release were not imported or missing from the release","messages":[]},{"title":"Lanterns.S01E08.1080p.WEB-DL.RGzsRutracker.mkv","messages":["Not a Custom Format upgrade for existing episode file(s). New: [] (0) do not improve on Existing: [2160p, DV Boost, HDR] (1800)"]}],"downloadId":"BF25AB39B7404F414EDE90B2F46717FE1CDEBC34","outputPath":"/mnt/seedbox/sonarr/Lanterns (Season 1) WEB-DL 1080p","id":852984238,"episode":{"id":737,"seasonNumber":1,"episodeNumber":8,"absoluteEpisodeNumber":null,"title":"Dirt and Stars","airDate":"2026-10-04"}}`
	sonarrFile   = `{"path":"/mnt/seedbox/sonarr/Lanterns.S01.AMZN.SDR.WEB-DL.2160p.by.AKTEP/Lanterns.S01E04.The.Weenie.AMZN.SDR.WEB-DL.2160p.by.AKTEP.mkv","relativePath":"Lanterns.S01E04.The.Weenie.AMZN.SDR.WEB-DL.2160p.by.AKTEP.mkv","folderName":"Lanterns.S01.AMZN.SDR.WEB-DL.2160p.by.AKTEP","size":7835891432,"seasonNumber":1,"episodes":[{"id":733,"seasonNumber":1,"episodeNumber":4,"absoluteEpisodeNumber":null,"title":"The Weenie","airDate":"2026-09-06"}],"quality":{"quality":{"id":18,"name":"WEBDL-2160p","source":"web","resolution":2160},"revision":{"version":1,"real":0,"isRepack":false}},"languages":[{"id":11,"name":"Russian"},{"id":30,"name":"Ukrainian"},{"id":1,"name":"English"}],"downloadId":"A9DFB07005483657874F48BC701C25833E822889","indexerFlags":0,"releaseType":"seasonPack","rejections":[],"id":2052582707,"series":{"id":17,"title":"Lanterns","year":2026}}`
)

func TestDecode(t *testing.T) {
	var r QueueRecord
	if err := json.Unmarshal([]byte(radarrRecord), &r); err != nil {
		t.Fatal(err)
	}
	if r.MovieID != 3 || r.TrackedDownloadState != "importPending" || !KnownQuality(r.Quality) || !KnownLanguages(r.Languages) {
		t.Fatalf("%+v", r)
	}
	var f ImportItem
	if err := json.Unmarshal([]byte(radarrFile), &f); err != nil {
		t.Fatal(err)
	}
	if f.Movie == nil || f.Movie.ID != 3 || f.Rejections[0].Reason != "Unable to parse file" || KnownQuality(f.Quality) || KnownLanguages(f.Languages) {
		t.Fatalf("%+v", f)
	}
	if err := json.Unmarshal([]byte(sonarrRecord), &r); err != nil {
		t.Fatal(err)
	}
	if r.SeriesID != 17 || r.Episode == nil || r.Episode.EpisodeNumber != 8 || r.Episode.AirDate != "2026-10-04" || len(r.StatusMessages[0].Messages) != 0 {
		t.Fatalf("%+v", r)
	}
	if err := json.Unmarshal([]byte(sonarrFile), &f); err != nil {
		t.Fatal(err)
	}
	if f.Series.ID != 17 || *f.SeasonNumber != 1 || f.Episodes[0].ID != 733 || f.ReleaseType != "seasonPack" || f.FolderName == "" {
		t.Fatalf("%+v", f)
	}
}

func TestQueuePages(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("X-Api-Key") != "key" || r.URL.Query().Get("includeEpisode") != "true" {
			http.Error(w, "bad request", http.StatusBadRequest)
			return
		}
		records := []json.RawMessage{json.RawMessage(sonarrRecord)}
		if r.URL.Query().Get("page") == "3" {
			records = nil
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"totalRecords": 2, "records": records})
	}))
	defer srv.Close()
	c := &Client{Kind: Sonarr, URL: srv.URL + "/", Key: "key", HTTP: srv.Client()}
	rs, err := c.Queue(context.Background())
	if err != nil || len(rs) != 2 {
		t.Fatalf("%d records, %v", len(rs), err)
	}
	c.Key = "wrong"
	_, err = c.Queue(context.Background())
	if e, ok := err.(*Error); !ok || e.Status != http.StatusBadRequest {
		t.Fatalf("err %v", err)
	}
}
