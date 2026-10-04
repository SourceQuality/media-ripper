// Package odm writes Optical Disc Manifests (.odm.json), TheDiscDB's
// tool-neutral record of what is on a disc, used to contribute discs to the
// catalogue (https://github.com/TheDiscDb/optical-disc-manifest, schema v1).
// A manifest records what was observed, not what the titles are.
package odm

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"path"
	"sort"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"
)

// Manifest is schema v1. Every object is closed: only these fields.
type Manifest struct {
	Schema        string    `json:"$schema,omitempty"`
	SchemaVersion int       `json:"schemaVersion"`
	Producer      Producer  `json:"producer"`
	CapturedAt    time.Time `json:"capturedAt,omitempty"`
	Disc          Disc      `json:"disc"`
}

type Producer struct {
	Name    string `json:"name"`
	Version string `json:"version"`
	URI     string `json:"uri,omitempty"`
}

type Disc struct {
	Format      string       `json:"format"` // dvd | blu-ray | uhd-blu-ray | unknown
	Name        string       `json:"name,omitempty"`
	Identifiers []Identifier `json:"identifiers"`
	Files       []File       `json:"files,omitempty"`
	Titles      []Title      `json:"titles,omitempty"`
}

type Identifier struct {
	Kind  string `json:"kind"`
	Value string `json:"value"`
}

type File struct {
	Path      string `json:"path"`
	SizeBytes int64  `json:"sizeBytes"`
}

type Title struct {
	Source          TitleSource `json:"source"`
	DurationSeconds float64     `json:"durationSeconds,omitempty"`
	SizeBytes       int64       `json:"sizeBytes,omitempty"`
	ChapterCount    int         `json:"chapterCount,omitempty"`
}

// TitleSource locates a title: the playlist (or clip) on a Blu-ray, the
// title number on a DVD.
type TitleSource struct {
	Path  string `json:"path,omitempty"`  // "BDMV/PLAYLIST/00800.mpls"
	Title int    `json:"title,omitempty"` // DVD title 1-99
}

// BlurayTitleSource turns MakeMKV's source file name ("00800.mpls",
// "00001.m2ts") into the manifest's path, or false when it is neither.
func BlurayTitleSource(file string) (TitleSource, bool) {
	switch strings.ToLower(path.Ext(file)) {
	case ".mpls":
		return TitleSource{Path: "BDMV/PLAYLIST/" + file}, true
	case ".m2ts":
		return TitleSource{Path: "BDMV/STREAM/" + file}, true
	}
	return TitleSource{}, false
}

// SchemaURL is the published v1 schema.
const SchemaURL = "https://raw.githubusercontent.com/TheDiscDb/optical-disc-manifest/main/schemas/v1/optical-disc-manifest.schema.json"

// New assembles a manifest. contentHash must be the thediscdb-content-hash
// of files (the spec requires exactly one).
func New(producerVersion, format, name, contentHash string, files []File, titles []Title, captured time.Time) (*Manifest, error) {
	if contentHash == "" {
		return nil, errors.New("odm: a content hash is required")
	}
	if len(files) == 0 {
		return nil, errors.New("odm: the disc's file inventory is required")
	}
	switch format {
	case "dvd", "blu-ray", "uhd-blu-ray", "unknown":
	default:
		format = "unknown"
	}
	ids := []Identifier{{Kind: "thediscdb-content-hash", Value: strings.ToUpper(contentHash)}}
	if m := Matrix256(files); m != "" {
		ids = append(ids, Identifier{Kind: "matrix256", Value: m})
	}
	fs := append([]File(nil), files...)
	sort.Slice(fs, func(i, j int) bool { return fs[i].Path < fs[j].Path })
	var ts []Title
	for _, t := range titles {
		if t.Source != (TitleSource{}) {
			ts = append(ts, t)
		}
	}
	return &Manifest{Schema: SchemaURL, SchemaVersion: 1, Producer: Producer{Name: "media-ripper", Version: producerVersion, URI: "https://github.com/sourcequality/media-ripper"},
		CapturedAt: captured.UTC().Truncate(time.Second), Disc: Disc{Format: format, Name: name, Identifiers: ids, Files: fs, Titles: ts}}, nil
}

// Matrix256 fingerprints the whole file inventory (matrix256 v1): each
// path's bytes, a NUL, the size in decimal, a newline, in byte order of
// the paths, hashed with SHA-256. The spec NFC-normalizes paths first; disc
// paths are ASCII in practice, and any that is not returns "" (omitted, as
// the identifier is only recommended) rather than risk a wrong value.
func Matrix256(files []File) string {
	paths := make([]File, 0, len(files))
	for _, f := range files {
		for _, r := range f.Path {
			if r >= utf8.RuneSelf {
				return ""
			}
		}
		paths = append(paths, f)
	}
	sort.Slice(paths, func(i, j int) bool { return paths[i].Path < paths[j].Path })
	h := sha256.New()
	for _, f := range paths {
		h.Write([]byte(f.Path))
		h.Write([]byte{0})
		h.Write([]byte(strconv.FormatInt(f.SizeBytes, 10)))
		h.Write([]byte{'\n'})
	}
	return hex.EncodeToString(h.Sum(nil))
}
