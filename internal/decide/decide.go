// Package decide holds the deterministic rules unstuckarr imports by: which
// of the application's rejections a manual import overrules, which files of
// a download are worth importing, which file is a movie's feature, and which
// episodes a file of a series download is.
package decide

import (
	"path"
	"slices"
	"strings"
)

// overrulable are the rejections and warnings, matched case-insensitively
// as substrings, that say the application could not tell what a file or a
// download is, or would not say so without a human. Radarr and Sonarr word
// them so.
var overrulable = []string{
	"unable to parse file",
	"unable to parse download",
	"unknown movie",
	"unknown series",
	"invalid movie",
	"episodes not selected",
	"invalid season or episode",
	"does not have a title and recently aired",
	"has a tba title and recently aired",
	"title mismatch",
	"was matched to movie by id",
	"was matched to series by id",
	"automatic import is not possible",
	"manual import required",
}

// headers are status message titles that carry no reason of their own.
var headers = []string{
	"one or more movies expected in this release were not imported or missing",
	"one or more episodes expected in this release were not imported or missing",
}

// Overrulable reports whether a manual import with the movie or episodes the
// download was grabbed for overrules the reason.
func Overrulable(reason string) bool {
	r := strings.ToLower(reason)
	for _, o := range overrulable {
		if strings.Contains(r, o) {
			return true
		}
	}
	return false
}

// Header reports whether a status message title with no messages is a
// summary line rather than a reason.
func Header(title string) bool {
	t := strings.ToLower(title)
	for _, h := range headers {
		if strings.HasPrefix(t, h) {
			return true
		}
	}
	return false
}

var videoExt = map[string]bool{
	".mkv": true, ".mp4": true, ".m4v": true, ".avi": true, ".mov": true, ".wmv": true, ".ts": true,
	".m2ts": true, ".webm": true, ".mpg": true, ".mpeg": true, ".vob": true, ".ogm": true, ".ogv": true,
	".divx": true, ".flv": true, ".mk3d": true, ".iso": true, ".img": true,
}

var discExt = map[string]bool{".iso": true, ".img": true, ".vob": true, ".ifo": true}

var discDirs = map[string]bool{"bdmv": true, "video_ts": true, "hvdvd_ts": true}

// extraDirs are folders whose files are not the feature or an episode.
var extraDirs = map[string]bool{
	"extras": true, "extra": true, "featurettes": true, "featurette": true, "bonus": true, "bonuses": true,
	"behind the scenes": true, "deleted scenes": true, "interviews": true, "scenes": true, "samples": true,
	"sample": true, "shorts": true, "trailers": true, "trailer": true, "other": true,
	"special features": true, "бонусы": true, "доп. материалы": true, "дополнительные материалы": true,
}

// extraSuffixes end the base name of an extra, or are all of it, as Plex
// names them; a sample's may also start with "sample".
var extraSuffixes = []string{"sample", "trailer", "featurette", "behindthescenes", "deleted", "deletedscene", "interview", "ncop", "nced"}

// Disc reports whether the path is part of a Blu-ray or DVD disc structure
// or a disc image.
func Disc(p string) bool {
	p = strings.ReplaceAll(p, `\`, "/")
	if discExt[strings.ToLower(path.Ext(p))] {
		return true
	}
	for d := range strings.SplitSeq(path.Dir(p), "/") {
		if discDirs[strings.ToLower(d)] {
			return true
		}
	}
	return false
}

// Skip returns why a file of the given relative path and size is not a
// candidate for import, or "" when it is. A disc structure is not skipped
// here: Disc tells it.
func Skip(rel string, size, minSize int64) string {
	rel = strings.ReplaceAll(rel, `\`, "/")
	ext := strings.ToLower(path.Ext(rel))
	if !videoExt[ext] {
		return "not a video file"
	}
	for d := range strings.SplitSeq(path.Dir(rel), "/") {
		if extraDirs[strings.ToLower(d)] {
			return "in an extras folder"
		}
	}
	base := strings.ToLower(strings.TrimSuffix(path.Base(rel), path.Ext(rel)))
	for _, s := range extraSuffixes {
		if base == s {
			return "named as an extra or sample"
		}
		if i := len(base) - len(s); i > 0 && strings.HasSuffix(base, s) && strings.ContainsRune(".-_ ", rune(base[i-1])) {
			return "named as an extra or sample"
		}
	}
	if strings.HasPrefix(base, "sample") && len(base) > len("sample") && strings.ContainsRune(".-_ ", rune(base[len("sample")])) {
		return "named as an extra or sample"
	}
	if size < minSize {
		return "smaller than the minimum size"
	}
	return ""
}

// Feature returns the index of the movie's feature among the sizes of its
// candidate files: the only one, or the largest when it is at least twice
// the next. ok is false when none stands out.
func Feature(sizes []int64) (i int, ok bool) {
	switch len(sizes) {
	case 0:
		return 0, false
	case 1:
		return 0, true
	}
	order := make([]int, len(sizes))
	for i := range order {
		order[i] = i
	}
	slices.SortStableFunc(order, func(a, b int) int {
		switch {
		case sizes[a] > sizes[b]:
			return -1
		case sizes[a] < sizes[b]:
			return 1
		}
		return 0
	})
	if sizes[order[0]] >= 2*sizes[order[1]] {
		return order[0], true
	}
	return 0, false
}
