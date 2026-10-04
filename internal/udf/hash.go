package udf

import (
	"crypto/md5"
	"encoding/binary"
	"encoding/hex"
	"errors"
	"path"
	"sort"
	"strings"
)

// ContentHash computes TheDiscDB's content hash: the MD5 of the sizes of the
// disc's video files, as little-endian int64s ordered by file name. It
// identifies a disc even when the volume label says nothing ("BD_ROM").
func ContentHash(files []File) (string, error) {
	dvd := false
	for _, f := range files {
		if hasPrefixFold(f.Path, "VIDEO_TS/") {
			dvd = true
			break
		}
	}
	type sized struct {
		name string
		size int64
	}
	var picked []sized
	for _, f := range files {
		if dvd {
			if !hasPrefixFold(f.Path, "VIDEO_TS/") ||
				!(hasSuffixFold(f.Path, ".VOB") || hasSuffixFold(f.Path, ".IFO") || hasSuffixFold(f.Path, ".BUP")) {
				continue
			}
		} else if !hasPrefixFold(f.Path, "BDMV/STREAM/") || !hasSuffixFold(f.Path, ".m2ts") {
			continue
		}
		picked = append(picked, sized{path.Base(f.Path), f.Size})
	}
	if len(picked) == 0 {
		return "", errors.New("udf: no video files to hash")
	}
	// Plain string order is ordinal (byte-value) order.
	sort.Slice(picked, func(i, j int) bool { return picked[i].name < picked[j].name })
	buf := make([]byte, 0, 8*len(picked))
	for _, p := range picked {
		buf = binary.LittleEndian.AppendUint64(buf, uint64(p.size))
	}
	sum := md5.Sum(buf)
	return strings.ToUpper(hex.EncodeToString(sum[:])), nil
}

func hasPrefixFold(s, prefix string) bool {
	return len(s) >= len(prefix) && strings.EqualFold(s[:len(prefix)], prefix)
}

func hasSuffixFold(s, suffix string) bool {
	return len(s) >= len(suffix) && strings.EqualFold(s[len(s)-len(suffix):], suffix)
}
