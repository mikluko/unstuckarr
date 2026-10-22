package decide

import (
	"path"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"unicode"

	"github.com/mikluko/unstuckarr/internal/arr"
)

// Sources of a file's episodes.
const (
	FromArr          = "arr"
	FromName         = "name"
	FromSeasonNumber = "season-number"
	FromAbsolute     = "absolute"
	FromAirDate      = "air-date"
	FromLLM          = "llm"
)

// EpisodeFile is a file of a series download.
type EpisodeFile struct {
	// Path is relative to the download.
	Path string
	// Proposed are the episodes the application matched the file to.
	Proposed []int
}

// Match is the episodes a file is, and how they were found. A file with
// neither EpisodeIDs nor Reason is unresolved.
type Match struct {
	EpisodeIDs []int
	Source     string
	// Reason is why the file is not to be matched at all.
	Reason string
}

// Episodes matches each file to episodes among allowed: by the
// application's proposal, else by SxxEyy in its name, else by a number in
// its name as the episode of the only season allowed, else as an absolute
// number, else by an air date in its name. A match outside allowed is no
// match, and episodes matched to more than one file leave each of those
// files unresolved. A proposal outside allowed sets Reason.
func Episodes(files []EpisodeFile, allowed []arr.Episode) []Match {
	byID := map[int]bool{}
	bySE := map[[2]int]int{}
	byAbs := map[int]int{}
	byDate := map[string]int{}
	seasons := map[int]bool{}
	for _, e := range allowed {
		byID[e.ID] = true
		bySE[[2]int{e.SeasonNumber, e.EpisodeNumber}] = e.ID
		if e.AbsoluteEpisodeNumber > 0 {
			byAbs[e.AbsoluteEpisodeNumber] = e.ID
		}
		if e.AirDate != "" {
			byDate[e.AirDate] = e.ID
		}
		seasons[e.SeasonNumber] = true
	}

	out := make([]Match, len(files))
	var pending []int
	for i, f := range files {
		if len(f.Proposed) > 0 {
			if all(f.Proposed, func(id int) bool { return byID[id] }) {
				out[i] = Match{EpisodeIDs: sorted(f.Proposed), Source: FromArr}
			} else {
				out[i].Reason = "the application matched it to episodes the download was not grabbed for"
			}
			continue
		}
		if ses := seasonEpisodes(baseName(f.Path)); len(ses) > 0 {
			var ids []int
			for _, se := range ses {
				if id, ok := bySE[se]; ok {
					ids = append(ids, id)
				}
			}
			if len(ids) == len(ses) {
				out[i] = Match{EpisodeIDs: sorted(ids), Source: FromName}
				continue
			}
		}
		pending = append(pending, i)
	}

	names := make([]string, len(pending))
	for k, i := range pending {
		names[k] = baseName(files[i].Path)
	}
	numbers := bareNumbers(names)
	for k, i := range pending {
		if n := numbers[k]; n > 0 {
			if len(seasons) == 1 {
				for s := range seasons {
					if id, ok := bySE[[2]int{s, n}]; ok {
						out[i] = Match{EpisodeIDs: []int{id}, Source: FromSeasonNumber}
					}
				}
				if out[i].Source != "" {
					continue
				}
			}
			if id, ok := byAbs[n]; ok {
				out[i] = Match{EpisodeIDs: []int{id}, Source: FromAbsolute}
				continue
			}
		}
		if d := airDate(names[k]); d != "" {
			if id, ok := byDate[d]; ok {
				out[i] = Match{EpisodeIDs: []int{id}, Source: FromAirDate}
			}
		}
	}

	Unclaim(out)
	return out
}

// Unclaim leaves unresolved every match that shares an episode with
// another.
func Unclaim(ms []Match) {
	claims := map[int]int{}
	for _, m := range ms {
		for _, id := range m.EpisodeIDs {
			claims[id]++
		}
	}
	for i, m := range ms {
		if slices.ContainsFunc(m.EpisodeIDs, func(id int) bool { return claims[id] > 1 }) {
			ms[i] = Match{}
		}
	}
}

func all(ids []int, f func(int) bool) bool {
	for _, id := range ids {
		if !f(id) {
			return false
		}
	}
	return true
}

func sorted(ids []int) []int {
	s := slices.Clone(ids)
	slices.Sort(s)
	return slices.Compact(s)
}

func baseName(p string) string {
	p = path.Base(strings.ReplaceAll(p, `\`, "/"))
	return strings.ToLower(strings.TrimSuffix(p, path.Ext(p)))
}

var (
	sxxeyy    = regexp.MustCompile(`(?:^|[^\pL\pN])s(\d{1,3})[ ._-]?e(\d{1,4})((?:[ ._-]?-?[ ._-]?e\d{1,4})*)`)
	moreEps   = regexp.MustCompile(`e(\d{1,4})`)
	nxnn      = regexp.MustCompile(`(?:^|[^\pL\pN])(\d{1,2})x(\d{2,3})(?:[^\pL\pN]|$)`)
	worded    = regexp.MustCompile(`(?:season|сезон)[ ._-]*(\d{1,2}).*?(?:episode|серия|эпизод)[ ._-]*(\d{1,4})`)
	dateRe    = regexp.MustCompile(`((?:19|20)\d{2})[ ._-](\d{2})[ ._-](\d{2})`)
	strongNum = regexp.MustCompile(`^(?:e|ep)(\d{1,3})$`)
)

// seasonEpisodes returns the season and episode numbers a lower-cased name
// spells out, or none.
func seasonEpisodes(name string) [][2]int {
	if m := sxxeyy.FindStringSubmatch(name); m != nil {
		s, _ := strconv.Atoi(m[1])
		e, _ := strconv.Atoi(m[2])
		out := [][2]int{{s, e}}
		for _, x := range moreEps.FindAllStringSubmatch(m[3], -1) {
			n, _ := strconv.Atoi(x[1])
			out = append(out, [2]int{s, n})
		}
		return out
	}
	for _, re := range []*regexp.Regexp{nxnn, worded} {
		if m := re.FindStringSubmatch(name); m != nil {
			s, _ := strconv.Atoi(m[1])
			e, _ := strconv.Atoi(m[2])
			return [][2]int{{s, e}}
		}
	}
	return nil
}

// airDate returns the YYYY-MM-DD date a lower-cased name carries, or "".
func airDate(name string) string {
	m := dateRe.FindStringSubmatch(name)
	if m == nil {
		return ""
	}
	return m[1] + "-" + m[2] + "-" + m[3]
}

type token struct {
	text string
	// sep is the separator before the token, or "".
	sep string
}

func tokens(name string) []token {
	var out []token
	var cur, sep strings.Builder
	flush := func() {
		if cur.Len() > 0 {
			out = append(out, token{text: cur.String(), sep: sep.String()})
			cur.Reset()
			sep.Reset()
		}
	}
	for _, r := range name {
		if unicode.IsLetter(r) || unicode.IsDigit(r) {
			cur.WriteRune(r)
			continue
		}
		flush()
		sep.WriteRune(r)
	}
	flush()
	return out
}

// numberCandidates returns the numbers in a lower-cased name that may be an
// episode's: up to three digits, not part of a date, an audio channel layout
// such as 5.1, or a codec such as h.264. Numbers written as E05 or Ep05,
// when there are any, are the only candidates.
func numberCandidates(name string) []int {
	name = dateRe.ReplaceAllString(name, " ")
	ts := tokens(name)
	var plain, strong []int
	for i, t := range ts {
		if m := strongNum.FindStringSubmatch(t.text); m != nil {
			n, _ := strconv.Atoi(m[1])
			strong = append(strong, n)
			continue
		}
		if len(t.text) > 3 || strings.IndexFunc(t.text, func(r rune) bool { return !unicode.IsDigit(r) }) >= 0 {
			continue
		}
		if i > 0 {
			prev := ts[i-1].text
			if len(t.text) == 1 && t.sep == "." && unicode.IsDigit(rune(prev[len(prev)-1])) && strings.IndexFunc(prev, unicode.IsLetter) >= 0 {
				continue
			}
			if (prev == "h" || prev == "x") && (t.text == "264" || t.text == "265" || t.text == "266") {
				continue
			}
		}
		n, _ := strconv.Atoi(t.text)
		plain = append(plain, n)
	}
	if len(strong) > 0 {
		return strong
	}
	return plain
}

// bareNumbers returns the episode number each lower-cased name carries, or
// 0. Across several names that have the same count of candidates, the
// number is the one at the only position whose values all differ.
// Otherwise a name's number is its only candidate.
func bareNumbers(names []string) []int {
	cands := make([][]int, len(names))
	for i, n := range names {
		cands[i] = numberCandidates(n)
	}
	out := make([]int, len(names))
	if len(names) > 1 && len(cands[0]) > 0 && all(indices(cands), func(i int) bool { return len(cands[i]) == len(cands[0]) }) {
		pos := -1
		for j := range cands[0] {
			seen := map[int]bool{}
			distinct := true
			for _, c := range cands {
				if seen[c[j]] {
					distinct = false
					break
				}
				seen[c[j]] = true
			}
			if distinct {
				if pos >= 0 {
					pos = -2
					break
				}
				pos = j
			}
		}
		if pos >= 0 {
			for i, c := range cands {
				out[i] = c[pos]
			}
			return out
		}
	}
	for i, c := range cands {
		if len(c) == 1 {
			out[i] = c[0]
		}
	}
	return out
}

func indices[T any](s []T) []int {
	out := make([]int, len(s))
	for i := range out {
		out[i] = i
	}
	return out
}
