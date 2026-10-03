// Package naming renders output path templates and sanitises path components.
package naming

import (
	"fmt"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"time"
)

// Vars are the values available to a template.
type Vars struct {
	Title        string
	Year         int
	Series       string
	Season       int
	Episode      int
	EpisodeEnd   int
	EpisodeTitle string
	Label        string
	TitleID      int
	Resolution   string
	Date         time.Time
	Source       string // bluray / dvd
}

var reToken = regexp.MustCompile(`\{([a-z_]+)(?::(\d+))?\}`)

// Render expands {token} and {token:02} placeholders, sanitises each path
// component and returns a relative path.
func Render(tmpl string, v Vars) (string, error) {
	tmpl = filepath.ToSlash(strings.Trim(tmpl, "/"))
	parts := strings.Split(tmpl, "/")
	var out []string
	for i, part := range parts {
		isLast := i == len(parts)-1
		s := reToken.ReplaceAllStringFunc(part, func(m string) string {
			sub := reToken.FindStringSubmatch(m)
			width := 0
			if sub[2] != "" {
				width, _ = strconv.Atoi(sub[2])
			}
			return v.value(sub[1], width)
		})
		s = cleanup(s)
		if isLast && strings.TrimSuffix(s, filepath.Ext(s)) == "" {
			return "", fmt.Errorf("template %q produced an empty file name", tmpl)
		}
		if s == "" {
			continue
		}
		out = append(out, Sanitize(s))
	}
	if len(out) == 0 {
		return "", fmt.Errorf("template %q produced an empty path", tmpl)
	}
	return filepath.Join(out...), nil
}

func (v Vars) value(name string, width int) string {
	pad := func(n int) string {
		if width > 0 {
			return fmt.Sprintf("%0*d", width, n)
		}
		return strconv.Itoa(n)
	}
	switch name {
	case "title":
		return v.Title
	case "year":
		if v.Year == 0 {
			return ""
		}
		return strconv.Itoa(v.Year)
	case "series":
		if v.Series != "" {
			return v.Series
		}
		return v.Title
	case "season":
		return pad(v.Season)
	case "episode":
		if v.EpisodeEnd > v.Episode {
			return pad(v.Episode) + "-E" + pad(v.EpisodeEnd)
		}
		return pad(v.Episode)
	case "episode_title":
		return v.EpisodeTitle
	case "label":
		return v.Label
	case "title_id":
		return pad(v.TitleID)
	case "resolution":
		return v.Resolution
	case "source":
		return v.Source
	case "date":
		d := v.Date
		if d.IsZero() {
			d = time.Now()
		}
		return d.Format("2006-01-02")
	case "datetime":
		d := v.Date
		if d.IsZero() {
			d = time.Now()
		}
		return d.Format("2006-01-02 15-04")
	}
	return ""
}

// cleanup removes artefacts left by empty tokens: "Title ()" and trailing
// " - " separators.
func cleanup(s string) string {
	s = strings.ReplaceAll(s, " ()", "")
	s = strings.ReplaceAll(s, "()", "")
	s = strings.ReplaceAll(s, " []", "")
	s = strings.ReplaceAll(s, "[]", "")
	for {
		t := s
		s = strings.ReplaceAll(s, "  ", " ")
		s = strings.ReplaceAll(s, " - .", ".")
		s = strings.ReplaceAll(s, " -.", ".")
		s = strings.ReplaceAll(s, "- -", "-")
		s = strings.TrimSuffix(s, " -")
		s = strings.TrimPrefix(s, "- ")
		if t == s {
			break
		}
	}
	return strings.TrimSpace(s)
}

var reIllegal = regexp.MustCompile(`[<>:"/\\|?*\x00-\x1f]`)

// Sanitize makes a single path component safe on Linux, SMB and NTFS shares.
func Sanitize(s string) string {
	s = strings.ReplaceAll(s, ": ", " - ")
	s = strings.ReplaceAll(s, ":", "-")
	s = strings.ReplaceAll(s, "/", "-")
	s = reIllegal.ReplaceAllString(s, "")
	s = strings.Join(strings.Fields(s), " ")
	s = strings.Trim(s, " .")
	if s == "" {
		s = "untitled"
	}
	if len(s) > 200 {
		s = s[:200]
	}
	return s
}
