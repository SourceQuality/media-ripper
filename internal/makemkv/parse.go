package makemkv

import (
	"strconv"
	"strings"
	"time"
)

// Attribute ids from MakeMKV's apdefs.h (ap_iaXXX).
const (
	attrType           = 1
	attrName           = 2
	attrLangCode       = 3
	attrLangName       = 4
	attrCodecID        = 5
	attrCodecShort     = 6
	attrCodecLong      = 7
	attrChapterCount   = 8
	attrDuration       = 9
	attrDiskSize       = 10
	attrDiskSizeBytes  = 11
	attrBitrate        = 13
	attrAudioChannels  = 14
	attrAngleInfo      = 15
	attrSourceFileName = 16
	attrVideoSize      = 19
	attrVideoAspect    = 20
	attrVideoFrameRate = 21
	attrStreamFlags    = 22
	attrOriginalTitle  = 24
	attrSegmentsCount  = 25
	attrSegmentsMap    = 26
	attrOutputFileName = 27
	attrMetaLangCode   = 28
	attrVolumeName     = 32
	attrOrderWeight    = 33
	attrComment        = 49
)

// splitRobot splits one "KEY:v1,v2,"quoted, value"" line into its fields.
// Quoted strings may contain commas and backslash-escaped quotes.
func splitRobot(s string) []string {
	var out []string
	var cur strings.Builder
	inQuote := false
	for i := 0; i < len(s); i++ {
		c := s[i]
		switch {
		case inQuote && c == '\\' && i+1 < len(s):
			i++
			cur.WriteByte(s[i])
		case c == '"':
			inQuote = !inQuote
		case c == ',' && !inQuote:
			out = append(out, cur.String())
			cur.Reset()
		default:
			cur.WriteByte(c)
		}
	}
	out = append(out, cur.String())
	return out
}

func atoi(s string) int {
	n, _ := strconv.Atoi(strings.TrimSpace(s))
	return n
}

func atoi64(s string) int64 {
	n, _ := strconv.ParseInt(strings.TrimSpace(s), 10, 64)
	return n
}

// parseDuration parses "H:MM:SS" or "MM:SS".
func parseDuration(s string) time.Duration {
	parts := strings.Split(strings.TrimSpace(s), ":")
	var total int
	for _, p := range parts {
		total = total*60 + atoi(p)
	}
	return time.Duration(total) * time.Second
}

// parseSegments extracts the segment numbers from a segment map such as
// "1,2,3", "(1,2),3" or "100-105".
func parseSegments(s string) []int {
	var out []int
	for _, tok := range strings.FieldsFunc(s, func(r rune) bool {
		return r == ',' || r == '(' || r == ')' || r == ' '
	}) {
		if a, b, ok := strings.Cut(tok, "-"); ok {
			lo, hi := atoi(a), atoi(b)
			if hi >= lo && hi-lo < 10000 {
				for n := lo; n <= hi; n++ {
					out = append(out, n)
				}
				continue
			}
		}
		if n, err := strconv.Atoi(tok); err == nil {
			out = append(out, n)
		}
	}
	return out
}

// parseInfo consumes robot-mode lines from an "info" run and builds a Disc.
func parseInfo(lines []string) *Disc {
	d := &Disc{}
	titles := map[int]*Title{}
	get := func(id int) *Title {
		t, ok := titles[id]
		if !ok {
			t = &Title{ID: id}
			titles[id] = t
		}
		return t
	}
	for _, line := range lines {
		key, rest, ok := strings.Cut(line, ":")
		if !ok {
			continue
		}
		f := splitRobot(rest)
		switch key {
		case "MSG":
			if len(f) >= 4 {
				d.Messages = append(d.Messages, Message{Code: atoi(f[0]), Text: f[3]})
			}
		case "CINFO":
			if len(f) < 3 {
				continue
			}
			switch atoi(f[0]) {
			case attrType:
				d.Type = f[2]
			case attrName:
				d.Name = f[2]
			case attrVolumeName:
				d.Volume = f[2]
			case attrMetaLangCode:
				d.MetaLang = f[2]
			}
		case "TCOUNT":
			d.TitleCount = atoi(f[0])
		case "TINFO":
			if len(f) < 4 {
				continue
			}
			t := get(atoi(f[0]))
			v := f[3]
			switch atoi(f[1]) {
			case attrName:
				t.Name = v
			case attrChapterCount:
				t.Chapters = atoi(v)
			case attrDuration:
				t.Duration = parseDuration(v)
			case attrDiskSize:
				t.Size = v
			case attrDiskSizeBytes:
				t.SizeBytes = atoi64(v)
			case attrAngleInfo:
				t.AngleInfo = v
			case attrSourceFileName:
				t.SourceFile = v
			case attrSegmentsCount:
				t.SegmentsCount = atoi(v)
			case attrSegmentsMap:
				t.SegmentsMap = v
				t.Segments = parseSegments(v)
			case attrOutputFileName:
				t.OutputFile = v
			case attrOrderWeight:
				t.OrderWeight = atoi(v)
			case attrComment:
				t.Comment = v
			case attrOriginalTitle:
				t.OriginalID = atoi(v)
			}
		case "SINFO":
			if len(f) < 5 {
				continue
			}
			t := get(atoi(f[0]))
			sid := atoi(f[1])
			for len(t.Streams) <= sid {
				t.Streams = append(t.Streams, Stream{Index: len(t.Streams)})
			}
			s := &t.Streams[sid]
			v := f[4]
			switch atoi(f[2]) {
			case attrType:
				s.Type = v
			case attrName:
				s.Name = v
			case attrLangCode:
				s.Lang = v
			case attrLangName:
				s.LangName = v
			case attrCodecID:
				s.CodecID = v
			case attrCodecShort:
				s.Codec = v
			case attrCodecLong:
				s.CodecLong = v
			case attrBitrate:
				s.Bitrate = v
			case attrAudioChannels:
				s.Channels = atoi(v)
			case attrVideoSize:
				s.VideoSize = v
			case attrVideoAspect:
				s.Aspect = v
			case attrVideoFrameRate:
				s.FrameRate = v
			case attrStreamFlags:
				s.Flags = atoi(v)
			}
		}
	}
	for id := 0; id < len(titles)+d.TitleCount; id++ {
		if t, ok := titles[id]; ok {
			d.Titles = append(d.Titles, t)
		}
	}
	return d
}
