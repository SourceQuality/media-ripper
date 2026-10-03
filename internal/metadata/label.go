// Package metadata turns a disc label into a movie or TV identity.
package metadata

import (
	"regexp"
	"strconv"
	"strings"
	"time"
	"unicode"
)

func maxYear() int { return time.Now().Year() + 1 }

// Kind is what a disc holds.
type Kind string

const (
	KindUnknown Kind = "unknown"
	KindMovie   Kind = "movie"
	KindTV      Kind = "tv"
)

// Hint is what we can learn from the label alone.
type Hint struct {
	Raw     string `json:"raw"`
	Query   string `json:"query"`
	Year    int    `json:"year,omitempty"`
	Season  int    `json:"season,omitempty"`
	Disc    int    `json:"disc,omitempty"`
	LooksTV bool   `json:"looks_tv,omitempty"`
	Junk    bool   `json:"junk,omitempty"`
}

var (
	reSeasonDisc = regexp.MustCompile(`(?i)\b(?:S|SEASON|SERIES)[ _.-]*(\d{1,2})[ _.-]*(?:D|DISC|DISK)[ _.-]*(\d{1,2})\b`)
	reSeason     = regexp.MustCompile(`(?i)\b(?:SEASON|SERIES|SEAS|SSN|S)[ _.-]*(\d{1,2})\b`)
	reDisc       = regexp.MustCompile(`(?i)\b(?:DISC|DISK|D|DVD|VOL|VOLUME|PART)[ _.-]*(\d{1,2})\b`)
	reYear       = regexp.MustCompile(`^(.*\S)[ ]+((?:19|20)\d{2})$`)
	reJunkTokens = regexp.MustCompile(`(?i)\b(BD|BDROM|BD[ _-]?ROM|BLURAY|BLU[ _-]?RAY|DVD|DVD[ _-]?VIDEO|UHD|4K|1080P|2160P|WS|FS|NTSC|PAL|REGION[ _]?\w+|SPECIAL[ _]EDITION|COLLECTORS?[ _]EDITION|EXTENDED[ _]EDITION|LIMITED[ _]EDITION|ANNIVERSARY[ _]EDITION|DIRECTORS?[ _]CUT|THEATRICAL[ _]CUT|STEELBOOK|REMASTERED|THE[ _]COMPLETE[ _]SERIES|COMPLETE[ _]SERIES|BOX[ _]?SET|AC3|DTS|TRUEHD|ATMOS|MKV)\b`)
	reSpace      = regexp.MustCompile(`[\s_.]+`)
	reNonWord    = regexp.MustCompile(`[^\p{L}\p{N}' &!-]+`)
	reRelease    = regexp.MustCompile(`(?i)^(?:WB|WDSHE|SONY|SPHE|UNIVERSAL|UNI|PARAMOUNT|PAR|FOX|TCFHE|MGM|LIONSGATE|LGF|DISNEY|BVHE|CRITERION|SHOUT|ARROW|KINO)[_ -]`)
)

var junkLabels = map[string]bool{
	"": true, "BD_ROM": true, "BDROM": true, "BLURAY": true, "BLU-RAY": true, "BLU_RAY": true,
	"DVD_VIDEO": true, "DVDVIDEO": true, "DVD": true, "DVD_ROM": true, "LOGICAL_VOLUME_ID": true,
	"UNTITLED": true, "UNTITLED_DISC": true, "DISC": true, "MOVIE": true, "TITLE": true,
	"NEW": true, "MY_DISC": true, "UNKNOWN": true, "VIDEO": true, "DVD-VIDEO": true,
}

// ParseLabel extracts a search query and season/disc/year hints from a volume
// label such as "THE_MATRIX_1999", "FRIENDS_S3_D2" or "BAND_OF_BROTHERS_DISC_1".
func ParseLabel(label string) Hint {
	h := Hint{Raw: label}
	s := strings.TrimSpace(label)
	if junkLabels[strings.ToUpper(strings.ReplaceAll(s, " ", "_"))] {
		h.Junk = true
		return h
	}
	s = reRelease.ReplaceAllString(s, "")
	s = reSpace.ReplaceAllString(s, " ")

	if m := reSeasonDisc.FindStringSubmatch(s); m != nil {
		h.Season, _ = strconv.Atoi(m[1])
		h.Disc, _ = strconv.Atoi(m[2])
		h.LooksTV = true
		s = strings.Replace(s, m[0], " ", 1)
	}
	if m := reSeason.FindStringSubmatch(s); m != nil && h.Season == 0 {
		// "S1" alone is ambiguous, require a longer word or an explicit disc.
		word := strings.ToUpper(m[0])
		if !strings.HasPrefix(word, "S"+m[1]) || reDisc.MatchString(s) {
			h.Season, _ = strconv.Atoi(m[1])
			h.LooksTV = true
			s = strings.Replace(s, m[0], " ", 1)
		}
	}
	if m := reDisc.FindStringSubmatch(s); m != nil && h.Disc == 0 {
		h.Disc, _ = strconv.Atoi(m[1])
		s = strings.Replace(s, m[0], " ", 1)
		if strings.HasPrefix(strings.ToUpper(strings.TrimSpace(m[0])), "DISC") || strings.HasPrefix(strings.ToUpper(strings.TrimSpace(m[0])), "DISK") {
			// A bare DISC number without a season is more often a TV set
			// than a two-disc movie, but we let the title list decide.
		}
	}
	s = reJunkTokens.ReplaceAllString(s, " ")
	s = strings.Join(strings.Fields(s), " ")
	// A trailing four-digit year is the release year; one at the start or in
	// the middle ("2001 A Space Odyssey", "Blade Runner 2049") is the title.
	if m := reYear.FindStringSubmatch(s); m != nil {
		if y, _ := strconv.Atoi(m[2]); y <= maxYear() {
			h.Year = y
			s = m[1]
		}
	}
	s = reNonWord.ReplaceAllString(s, " ")
	s = strings.Join(strings.Fields(s), " ")
	s = strings.Trim(s, " -")
	h.Query = TitleCase(s)
	if h.Query == "" {
		h.Junk = true
	}
	return h
}

var smallWords = map[string]bool{
	"a": true, "an": true, "and": true, "as": true, "at": true, "but": true, "by": true, "for": true,
	"in": true, "of": true, "on": true, "or": true, "the": true, "to": true, "vs": true, "with": true,
}

// TitleCase converts "THE LORD OF THE RINGS" to "The Lord of the Rings".
func TitleCase(s string) string {
	words := strings.Fields(strings.ToLower(s))
	for i, w := range words {
		if i > 0 && smallWords[w] {
			continue
		}
		r := []rune(w)
		r[0] = unicode.ToUpper(r[0])
		// Handle "o'brien" style apostrophes and hyphens lightly.
		for j := 1; j < len(r); j++ {
			if (r[j-1] == '\'' || r[j-1] == '-') && j < len(r) && r[j-1] == '-' {
				r[j] = unicode.ToUpper(r[j])
			}
		}
		words[i] = string(r)
	}
	return strings.Join(words, " ")
}

// Similarity scores how well a candidate title matches a query, 0..1.
func Similarity(query, candidate string) float64 {
	a := normalize(query)
	b := normalize(candidate)
	if a == "" || b == "" {
		return 0
	}
	if a == b {
		return 1
	}
	at := strings.Fields(a)
	bt := strings.Fields(b)
	set := map[string]bool{}
	for _, t := range bt {
		set[t] = true
	}
	common := 0
	for _, t := range at {
		if set[t] {
			common++
		}
	}
	if common == 0 {
		return 0
	}
	// Dice coefficient over tokens, with a bonus when one is a prefix of the other.
	score := 2 * float64(common) / float64(len(at)+len(bt))
	if strings.HasPrefix(b, a) || strings.HasPrefix(a, b) {
		score = (score + 1) / 2
	}
	return score
}

func normalize(s string) string {
	s = strings.ToLower(s)
	s = strings.ReplaceAll(s, "&", " and ")
	var b strings.Builder
	for _, r := range s {
		if unicode.IsLetter(r) || unicode.IsNumber(r) || r == ' ' {
			b.WriteRune(r)
		} else {
			b.WriteRune(' ')
		}
	}
	words := strings.Fields(b.String())
	out := words[:0]
	for _, w := range words {
		if w == "the" || w == "a" || w == "an" {
			continue
		}
		out = append(out, w)
	}
	return strings.Join(out, " ")
}
