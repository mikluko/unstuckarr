package decide

import (
	"slices"
	"testing"

	"github.com/mikluko/unstuckarr/internal/arr"
)

func TestOverrulable(t *testing.T) {
	for reason, want := range map[string]bool{
		"Unable to parse file": true,
		"Unable to parse download, automatic import is not possible.": true,
		"Unknown Movie":                                    true,
		"Unknown Series":                                   true,
		"Invalid season or episode":                        true,
		"Episodes not selected":                            true,
		"Episode has a TBA title and recently aired":       true,
		"Episode does not have a title and recently aired": true,
		"Found matching movie via grab history, but release was matched to movie by ID. Manual Import required.":                                      true,
		"Found matching series via grab history, but release was matched to series by ID. Automatic import is not possible. See the FAQ for details.": true,
		"Movie title mismatch, automatic import is not possible. Manual Import required.":                                                             true,
		"Failed to import episode, Destination already exists.":                                                                                       false,
		"Not an upgrade for existing movie file(s)":                                                                                                   false,
		"Episode file already imported at 10/19/2026 19:51:01":                                                                                        false,
		"Not a Custom Format upgrade for existing episode file(s). New: [] (0) do not improve on Existing: []":                                        false,
		"Sample": false,
		"No files found are eligible for import in /mnt/x": false,
	} {
		if got := Overrulable(reason); got != want {
			t.Errorf("Overrulable(%q) = %v, want %v", reason, got, want)
		}
	}
	if !Header("One or more episodes expected in this release were not imported or missing from the release") {
		t.Error("the Sonarr summary line is a header")
	}
	if Header("Unable to parse download, automatic import is not possible.") {
		t.Error("a reason is not a header")
	}
}

func TestSkip(t *testing.T) {
	const min = 50e6
	for rel, want := range map[string]string{
		"Kiki_Delivery_Service.mkv":         "",
		"Ponyo_on_the_Cliff_by_the_Sea.mkv": "",
		"Trailer.Park.Boys.S01E01.mkv":      "",
		"Bonus/BD Menu.mkv":                 "in an extras folder",
		"Extras/Making of.mkv":              "in an extras folder",
		"Movie.2008.1080p-sample.mkv":       "named as an extra or sample",
		"sample.mkv":                        "named as an extra or sample",
		"Movie.2008-trailer.mp4":            "named as an extra or sample",
		"Movie.2008.nfo":                    "not a video file",
		"Subs/Rus.srt":                      "not a video file",
		"tiny.mkv":                          "smaller than the minimum size",
	} {
		size := int64(1e9)
		if rel == "tiny.mkv" {
			size = 1e6
		}
		if got := Skip(rel, size, min); got != want {
			t.Errorf("Skip(%q) = %q, want %q", rel, got, want)
		}
	}
	for p, want := range map[string]bool{
		"Movie/BDMV/STREAM/00001.m2ts": true,
		"Movie/VIDEO_TS/VTS_01_1.VOB":  true,
		"Movie.iso":                    true,
		"Movie.mkv":                    false,
		"Movie/00001.m2ts":             false,
	} {
		if got := Disc(p); got != want {
			t.Errorf("Disc(%q) = %v, want %v", p, got, want)
		}
	}
}

func TestFeature(t *testing.T) {
	for _, c := range []struct {
		sizes []int64
		i     int
		ok    bool
	}{
		{nil, 0, false},
		{[]int64{5}, 0, true},
		{[]int64{1, 30, 4}, 1, true},
		{[]int64{20, 30}, 0, false},
		{[]int64{15, 30, 15}, 1, true},
	} {
		i, ok := Feature(c.sizes)
		if i != c.i || ok != c.ok {
			t.Errorf("Feature(%v) = %d, %v, want %d, %v", c.sizes, i, ok, c.i, c.ok)
		}
	}
}

func eps(season int, n ...int) []arr.Episode {
	var out []arr.Episode
	for _, e := range n {
		out = append(out, arr.Episode{ID: season*100 + e, SeasonNumber: season, EpisodeNumber: e, AbsoluteEpisodeNumber: 20 + e})
	}
	return out
}

func files(paths ...string) []EpisodeFile {
	out := make([]EpisodeFile, len(paths))
	for i, p := range paths {
		out[i] = EpisodeFile{Path: p}
	}
	return out
}

func ids(ms []Match) [][]int {
	out := make([][]int, len(ms))
	for i, m := range ms {
		out[i] = m.EpisodeIDs
	}
	return out
}

func TestEpisodes(t *testing.T) {
	for _, c := range []struct {
		name    string
		files   []EpisodeFile
		allowed []arr.Episode
		want    [][]int
		source  string
	}{
		{"proposal", []EpisodeFile{{Path: "x.mkv", Proposed: []int{101}}}, eps(1, 1, 2), [][]int{{101}}, FromArr},
		{"SxxEyy", files("Show.S01E02.1080p.mkv", "show s01e01e03.mkv"), eps(1, 1, 2, 3), [][]int{{102}, {101, 103}}, FromName},
		{"SxxEyy outside the grab", files("Show.S01E05.mkv"), eps(1, 1, 2), [][]int{nil}, ""},
		{"1x02", files("Show 1x02 Title.mkv"), eps(1, 1, 2), [][]int{{102}}, FromName},
		{"multi", files("Show.S01E01-E02.mkv"), eps(1, 1, 2, 3), [][]int{{101, 102}}, FromName},
		{"russian titles", files("01. Пилот.mkv", "02. Доверие.mkv", "03. Огни DDP5.1 H.265.mkv"), eps(1, 1, 2, 3), [][]int{{101}, {102}, {103}}, FromSeasonNumber},
		{"worded", files("Сезон 1 Серия 3.mkv"), eps(1, 3), [][]int{{103}}, FromName},
		{"varying position", files("Show 2 - 05 [1080p].mkv", "Show 2 - 06 [1080p].mkv"), eps(2, 5, 6), [][]int{{205}, {206}}, FromSeasonNumber},
		{"strong", files("Show 2 Ep05 720.mkv"), eps(2, 5), [][]int{{205}}, FromSeasonNumber},
		{"absolute", files("[Group] Show - 22 [ABCD1234].mkv"), eps(1, 2), [][]int{{102}}, FromAbsolute},
		{"air date", files("Show.2024.05.12.Guest.mkv"), []arr.Episode{{ID: 7, SeasonNumber: 2024, EpisodeNumber: 40, AirDate: "2024-05-12"}}, [][]int{{7}}, FromAirDate},
		{"conflict", files("Show.S01E01.mkv", "Show.S01E01.REPACK.mkv"), eps(1, 1), [][]int{nil, nil}, ""},
		{"no number", files("Пилот.mkv"), eps(1, 1), [][]int{nil}, ""},
	} {
		t.Run(c.name, func(t *testing.T) {
			got := Episodes(c.files, c.allowed)
			if !slices.EqualFunc(ids(got), c.want, slices.Equal) {
				t.Fatalf("got %v, want %v", ids(got), c.want)
			}
			if c.source != "" && got[0].Source != c.source {
				t.Fatalf("source %q, want %q", got[0].Source, c.source)
			}
		})
	}
	got := Episodes([]EpisodeFile{{Path: "x.mkv", Proposed: []int{999}}}, eps(1, 1))
	if got[0].Reason == "" || got[0].EpisodeIDs != nil {
		t.Fatalf("a proposal outside the grab: %+v", got[0])
	}
}
