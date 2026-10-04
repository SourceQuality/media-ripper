// Package udf lists the files on an optical disc's UDF filesystem by reading
// the raw device, so a disc can be identified from its file inventory without
// mounting it or shelling out. It is strictly read-only and treats every byte
// on the disc as untrusted: malformed structures produce errors, never panics.
//
// Supported: UDF 1.02 (DVD-Video) through 2.60 (Blu-ray), including the
// metadata partition, File Entries and Extended File Entries, short and long
// allocation descriptors, allocation extent chains and data embedded in the
// ICB. Not supported: the virtual allocation table used by incrementally
// written discs, and sparing tables (sparable partitions are read as plain
// physical partitions, which is right for discs nobody has rewritten).
package udf

import (
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"math"
	"os"
	"strings"
	"unicode/utf16"
)

// File is a regular file on the disc.
type File struct {
	Path string // '/'-separated, no leading slash, e.g. "BDMV/STREAM/00001.m2ts"
	Size int64
}

const (
	sectorSize = 2048

	// Limits that keep a hostile disc from making us loop or allocate wildly.
	maxDepth      = 32
	maxEntries    = 200000
	maxDirSize    = 16 << 20
	maxExtents    = 4096
	maxAEDHops    = 64
	maxIndirect   = 8
	maxVDSSectors = 64
)

// Descriptor tag identifiers (ECMA-167 3/7.2.1 and 4/7.2.1).
const (
	tagAVDP        = 2
	tagPartition   = 5
	tagLogicalVol  = 6
	tagTerminating = 8
	tagFileSet     = 256
	tagFileID      = 257
	tagAllocExtent = 258
	tagIndirect    = 259
	tagFileEntry   = 261
	tagExtFileEnt  = 266
)

// ICB file types (ECMA-167 4/14.6.6). Others (symlinks, streams) are skipped.
const (
	fileTypeDir     = 4
	fileTypeRegular = 5
)

// ICB allocation descriptor types (ECMA-167 4/14.6.8).
const (
	adShort    = 0
	adLong     = 1
	adExtended = 2
	adEmbedded = 3
)

// ListDevice opens path read-only and lists its files.
func ListDevice(path string) ([]File, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	// Block devices report a zero size from Stat, but seeking to the end
	// works; knowing the size lets List fall back to the trailing anchors.
	var r io.ReaderAt = f
	if size, err := f.Seek(0, io.SeekEnd); err == nil && size > 0 {
		r = io.NewSectionReader(f, 0, size)
	}
	return List(r)
}

// List returns every regular file on the UDF filesystem read through r.
func List(r io.ReaderAt) ([]File, error) {
	v := &volume{r: r, visited: map[uint64]bool{}}
	root, err := v.mount()
	if err != nil {
		return nil, err
	}
	if err := v.walk(root, "", 0); err != nil {
		return nil, err
	}
	return v.files, nil
}

type volume struct {
	r       io.ReaderAt
	parts   []partition // indexed by partition reference number; nil = unsupported
	visited map[uint64]bool
	entries int
	files   []File
}

// partition maps a logical block number to an absolute sector.
type partition interface {
	sector(block uint32) (int64, error)
}

type physical struct{ start, length uint32 }

func (p physical) sector(block uint32) (int64, error) {
	if block >= p.length {
		return 0, fmt.Errorf("udf: block %d outside partition of %d blocks", block, p.length)
	}
	return int64(p.start) + int64(block), nil
}

// metadataPart is a UDF 2.50+ metadata partition: its logical blocks are the
// blocks of the metadata file, which itself lives in a physical partition.
type metadataPart struct {
	phys physical
	runs []run
}

type run struct{ start, count uint32 }

func (m metadataPart) sector(block uint32) (int64, error) {
	b := block
	for _, r := range m.runs {
		if b < r.count {
			return m.phys.sector(r.start + b)
		}
		b -= r.count
	}
	return 0, fmt.Errorf("udf: block %d outside metadata partition", block)
}

func (v *volume) readSector(sector int64) ([]byte, error) {
	buf := make([]byte, sectorSize)
	n, err := v.r.ReadAt(buf, sector*sectorSize)
	if n == sectorSize {
		return buf, nil
	}
	if err == nil {
		err = io.ErrUnexpectedEOF
	}
	return nil, fmt.Errorf("udf: read sector %d: %w", sector, err)
}

func (v *volume) readBlock(p partition, block uint32) ([]byte, error) {
	s, err := p.sector(block)
	if err != nil {
		return nil, err
	}
	return v.readSector(s)
}

// checkTag validates a descriptor tag's checksum and identifier. CRCs are not
// checked: the checksum already rejects garbage, and pressed discs with a bad
// CRC would otherwise become unidentifiable.
func checkTag(b []byte, want uint16) error {
	if len(b) < 16 {
		return errors.New("udf: short descriptor")
	}
	var sum byte
	for i := 0; i < 16; i++ {
		if i != 4 {
			sum += b[i]
		}
	}
	if sum != b[4] {
		return errors.New("udf: bad descriptor tag checksum")
	}
	if id := le16(b); id != want {
		return fmt.Errorf("udf: descriptor tag %d, want %d", id, want)
	}
	return nil
}

func le16(b []byte) uint16 { return binary.LittleEndian.Uint16(b) }
func le32(b []byte) uint32 { return binary.LittleEndian.Uint32(b) }
func le64(b []byte) uint64 { return binary.LittleEndian.Uint64(b) }

// mount reads the anchor, volume descriptors and file set descriptor and
// returns the root directory's entry.
func (v *volume) mount() (*entry, error) {
	avdp, err := v.anchor()
	if err != nil {
		return nil, err
	}
	// Try the main volume descriptor sequence, then the reserve copy.
	var lvd []byte
	var pds map[uint16][]byte
	for _, off := range []int{16, 24} {
		length, loc := le32(avdp[off:]), le32(avdp[off+4:])
		pds, lvd, err = v.readVDS(loc, length)
		if err == nil {
			break
		}
	}
	if err != nil {
		return nil, err
	}
	if err := v.loadPartitions(lvd, pds); err != nil {
		return nil, err
	}

	fsdPart, fsdBlock, err := v.longAD(lvd[248:264])
	if err != nil {
		return nil, fmt.Errorf("udf: file set descriptor: %w", err)
	}
	fsd, err := v.readBlock(fsdPart, fsdBlock)
	if err != nil {
		return nil, err
	}
	if err := checkTag(fsd, tagFileSet); err != nil {
		return nil, fmt.Errorf("udf: file set descriptor: %w", err)
	}
	rootPart, rootBlock, err := v.longAD(fsd[400:416])
	if err != nil {
		return nil, fmt.Errorf("udf: root directory: %w", err)
	}
	v.visited[icbKey(rootPart, rootBlock)] = true
	root, err := v.readEntry(rootPart, rootBlock)
	if err != nil {
		return nil, fmt.Errorf("udf: root directory: %w", err)
	}
	if root.fileType != fileTypeDir {
		return nil, errors.New("udf: root is not a directory")
	}
	return root, nil
}

// anchor finds an Anchor Volume Descriptor Pointer: at sector 256 on any
// finished disc, else at the last sector or 256 before it.
func (v *volume) anchor() ([]byte, error) {
	locs := []int64{256}
	if s, ok := v.r.(interface{ Size() int64 }); ok {
		if last := s.Size()/sectorSize - 1; last > 256 {
			locs = append(locs, last, last-256)
		}
	}
	for _, loc := range locs {
		b, err := v.readSector(loc)
		if err == nil && checkTag(b, tagAVDP) == nil {
			return b, nil
		}
	}
	return nil, errors.New("udf: no anchor volume descriptor pointer")
}

// readVDS collects partition descriptors by partition number and the logical
// volume descriptor from one volume descriptor sequence.
func (v *volume) readVDS(loc, length uint32) (map[uint16][]byte, []byte, error) {
	n := length / sectorSize
	if n > maxVDSSectors {
		n = maxVDSSectors
	}
	pds := map[uint16][]byte{}
	var lvd []byte
	for i := uint32(0); i < n; i++ {
		b, err := v.readSector(int64(loc) + int64(i))
		if err != nil {
			break
		}
		id := le16(b)
		if checkTag(b, id) != nil {
			break
		}
		// Later copies with a higher sequence number supersede earlier ones.
		switch id {
		case tagPartition:
			num := le16(b[22:])
			if old, ok := pds[num]; !ok || le32(b[16:]) >= le32(old[16:]) {
				pds[num] = b
			}
		case tagLogicalVol:
			if lvd == nil || le32(b[16:]) >= le32(lvd[16:]) {
				lvd = b
			}
		}
		if id == tagTerminating {
			break
		}
	}
	if lvd == nil || len(pds) == 0 {
		return nil, nil, errors.New("udf: volume descriptor sequence lacks partition or logical volume descriptor")
	}
	return pds, lvd, nil
}

func (v *volume) loadPartitions(lvd []byte, pds map[uint16][]byte) error {
	if bs := le32(lvd[212:]); bs != sectorSize {
		return fmt.Errorf("udf: logical block size %d not supported", bs)
	}
	mapLen, nMaps := le32(lvd[264:]), le32(lvd[268:])
	if mapLen > sectorSize-440 || nMaps > 64 {
		return errors.New("udf: partition map table too large")
	}
	maps := lvd[440 : 440+mapLen]
	phys := func(num uint16) (physical, error) {
		pd, ok := pds[num]
		if !ok {
			return physical{}, fmt.Errorf("udf: no descriptor for partition %d", num)
		}
		return physical{start: le32(pd[188:]), length: le32(pd[192:])}, nil
	}
	for i := uint32(0); i < nMaps; i++ {
		if len(maps) < 2 || maps[1] < 2 || int(maps[1]) > len(maps) {
			return errors.New("udf: malformed partition map")
		}
		m := maps[:maps[1]]
		maps = maps[maps[1]:]
		switch {
		case m[0] == 1 && len(m) >= 6:
			p, err := phys(le16(m[4:]))
			if err != nil {
				return err
			}
			v.parts = append(v.parts, p)
		case m[0] == 2 && len(m) >= 64:
			p, err := phys(le16(m[38:]))
			if err != nil {
				return err
			}
			switch strings.TrimRight(string(m[5:28]), "\x00") {
			case "*UDF Metadata Partition":
				mp, err := v.loadMetadata(p, le32(m[40:]), le32(m[44:]))
				if err != nil {
					return err
				}
				v.parts = append(v.parts, mp)
			case "*UDF Sparable Partition":
				v.parts = append(v.parts, p)
			default:
				// Virtual partitions (VAT) are left unresolvable.
				v.parts = append(v.parts, nil)
			}
		default:
			return fmt.Errorf("udf: unknown partition map type %d", m[0])
		}
	}
	return nil
}

// loadMetadata reads the metadata file's extents, falling back to its mirror.
func (v *volume) loadMetadata(p physical, main, mirror uint32) (partition, error) {
	var err error
	for _, loc := range []uint32{main, mirror} {
		var e *entry
		if e, err = v.readEntry(p, loc); err != nil {
			continue
		}
		var exts []extent
		if exts, err = v.extents(e); err != nil {
			continue
		}
		mp := metadataPart{phys: p}
		for _, x := range exts {
			if x.typ != 0 {
				err = errors.New("udf: metadata file has unrecorded extent")
				break
			}
			mp.runs = append(mp.runs, run{start: x.block, count: (x.length + sectorSize - 1) / sectorSize})
		}
		if err == nil {
			return mp, nil
		}
	}
	return nil, fmt.Errorf("udf: metadata partition: %w", err)
}

// longAD resolves a long_ad's partition and block.
func (v *volume) longAD(b []byte) (partition, uint32, error) {
	ref := le16(b[8:])
	if int(ref) >= len(v.parts) || v.parts[ref] == nil {
		return nil, 0, fmt.Errorf("udf: unsupported partition reference %d", ref)
	}
	return v.parts[ref], le32(b[4:]), nil
}

func icbKey(p partition, block uint32) uint64 {
	// Partitions are comparable values; fold them into the key cheaply.
	h := uint64(block)
	switch p := p.(type) {
	case physical:
		h |= uint64(p.start) << 32
	case metadataPart:
		h |= 1<<63 | uint64(p.phys.start)<<32
	}
	return h
}

// entry is the part of a (Extended) File Entry the walk needs.
type entry struct {
	part     partition // where the entry lives; short_ads are relative to it
	fileType byte
	size     uint64
	alloc    int
	ads      []byte // allocation descriptors, or the data itself if embedded
}

func (v *volume) readEntry(p partition, block uint32) (*entry, error) {
	for hop := 0; hop <= maxIndirect; hop++ {
		b, err := v.readBlock(p, block)
		if err != nil {
			return nil, err
		}
		var hdr, lenEA, lenAD int
		switch le16(b) {
		case tagFileEntry:
			hdr, lenEA, lenAD = 176, int(le32(b[168:])), int(le32(b[172:]))
		case tagExtFileEnt:
			hdr, lenEA, lenAD = 216, int(le32(b[208:])), int(le32(b[212:]))
		case tagIndirect:
			// Strategy 4096 chains: follow to the real entry.
			if err := checkTag(b, tagIndirect); err != nil {
				return nil, err
			}
			if p, block, err = v.longAD(b[36:52]); err != nil {
				return nil, err
			}
			continue
		default:
			return nil, fmt.Errorf("udf: block %d is not a file entry", block)
		}
		if err := checkTag(b, le16(b)); err != nil {
			return nil, err
		}
		if lenEA < 0 || lenAD < 0 || lenEA > sectorSize || lenAD > sectorSize || hdr+lenEA+lenAD > sectorSize {
			return nil, errors.New("udf: file entry descriptor lengths out of range")
		}
		return &entry{
			part:     p,
			fileType: b[27],
			size:     le64(b[56:]),
			alloc:    int(le16(b[34:]) & 7),
			ads:      b[hdr+lenEA : hdr+lenEA+lenAD],
		}, nil
	}
	return nil, errors.New("udf: too many indirect entries")
}

type extent struct {
	part   partition
	block  uint32
	length uint32
	typ    uint32 // 0 recorded, 1 allocated only, 2 neither
}

// extents decodes an entry's allocation descriptors, following allocation
// extent descriptor chains.
func (v *volume) extents(e *entry) ([]extent, error) {
	var step int
	switch e.alloc {
	case adShort:
		step = 8
	case adLong:
		step = 16
	default:
		return nil, fmt.Errorf("udf: allocation type %d not supported here", e.alloc)
	}
	var out []extent
	ads := e.ads
	for hops := 0; ; hops++ {
		var next []byte
		for i := 0; i+step <= len(ads); i += step {
			l := le32(ads[i:])
			x := extent{typ: l >> 30, length: l & 0x3fffffff, part: e.part, block: le32(ads[i+4:])}
			if x.length == 0 {
				break
			}
			if step == 16 {
				p, b, err := v.longAD(ads[i : i+16])
				if err != nil {
					return nil, err
				}
				x.part, x.block = p, b
			}
			if x.typ == 3 {
				aed, err := v.readBlock(x.part, x.block)
				if err != nil {
					return nil, err
				}
				if err := checkTag(aed, tagAllocExtent); err != nil {
					return nil, err
				}
				n := le32(aed[20:])
				if n > sectorSize-24 {
					return nil, errors.New("udf: allocation extent descriptor too long")
				}
				next = aed[24 : 24+n]
				break
			}
			if len(out) >= maxExtents {
				return nil, errors.New("udf: too many extents")
			}
			out = append(out, x)
		}
		if next == nil {
			return out, nil
		}
		if hops >= maxAEDHops {
			return nil, errors.New("udf: allocation extent chain too long")
		}
		ads = next
	}
}

// readData reads a directory's contents.
func (v *volume) readData(e *entry) ([]byte, error) {
	if e.size > maxDirSize {
		return nil, fmt.Errorf("udf: directory of %d bytes too large", e.size)
	}
	size := int(e.size)
	if e.alloc == adEmbedded {
		if size > len(e.ads) {
			return nil, errors.New("udf: embedded data shorter than its size")
		}
		return e.ads[:size], nil
	}
	exts, err := v.extents(e)
	if err != nil {
		return nil, err
	}
	data := make([]byte, 0, size)
	for _, x := range exts {
		for off := uint32(0); off < x.length && len(data) < size; off += sectorSize {
			n := min(int(x.length-off), sectorSize, size-len(data))
			if x.typ != 0 {
				data = append(data, make([]byte, n)...)
				continue
			}
			b, err := v.readBlock(x.part, x.block+off/sectorSize)
			if err != nil {
				return nil, err
			}
			data = append(data, b[:n]...)
		}
	}
	if len(data) < size {
		return nil, errors.New("udf: directory extents shorter than its size")
	}
	return data, nil
}

// File characteristics bits of a File Identifier Descriptor.
const (
	fidDeleted = 1 << 2
	fidParent  = 1 << 3
)

func (v *volume) walk(dir *entry, prefix string, depth int) error {
	if depth >= maxDepth {
		return errors.New("udf: directory tree too deep")
	}
	data, err := v.readData(dir)
	if err != nil {
		return err
	}
	for len(data) > 0 {
		if len(data) < 38 {
			return errors.New("udf: truncated file identifier descriptor")
		}
		if err := checkTag(data, tagFileID); err != nil {
			return err
		}
		chars, lenFI, lenIU := data[18], int(data[19]), int(le16(data[36:]))
		if 38+lenIU+lenFI > len(data) {
			return errors.New("udf: file identifier descriptor overruns directory")
		}
		fid := data[:38+lenIU+lenFI]
		data = data[min((38+lenIU+lenFI+3)&^3, len(data)):]
		if chars&(fidDeleted|fidParent) != 0 {
			continue
		}
		if v.entries++; v.entries > maxEntries {
			return errors.New("udf: too many directory entries")
		}
		name, err := decodeName(fid[38+lenIU:])
		if err != nil {
			return err
		}
		p, block, err := v.longAD(fid[20:36])
		if err != nil {
			return fmt.Errorf("udf: %s%s: %w", prefix, name, err)
		}
		e, err := v.readEntry(p, block)
		if err != nil {
			return fmt.Errorf("udf: %s%s: %w", prefix, name, err)
		}
		switch e.fileType {
		case fileTypeDir:
			key := icbKey(p, block)
			if v.visited[key] {
				return fmt.Errorf("udf: %s%s: directory cycle", prefix, name)
			}
			v.visited[key] = true
			if err := v.walk(e, prefix+name+"/", depth+1); err != nil {
				return err
			}
		case fileTypeRegular:
			if e.size > math.MaxInt64 {
				return fmt.Errorf("udf: %s%s: size out of range", prefix, name)
			}
			v.files = append(v.files, File{Path: prefix + name, Size: int64(e.size)})
		}
	}
	return nil
}

// decodeName decodes an OSTA CS0 compressed unicode file identifier.
func decodeName(b []byte) (string, error) {
	if len(b) < 2 {
		return "", errors.New("udf: empty file name")
	}
	var name string
	switch b[0] {
	case 8:
		r := make([]rune, len(b)-1)
		for i, c := range b[1:] {
			r[i] = rune(c)
		}
		name = string(r)
	case 16:
		if len(b)%2 != 1 {
			return "", errors.New("udf: odd-length 16-bit file name")
		}
		u := make([]uint16, (len(b)-1)/2)
		for i := range u {
			u[i] = uint16(b[1+2*i])<<8 | uint16(b[2+2*i])
		}
		name = string(utf16.Decode(u))
	default:
		return "", fmt.Errorf("udf: unknown file name compression %d", b[0])
	}
	if name == "." || name == ".." || strings.ContainsAny(name, "/\x00") {
		return "", fmt.Errorf("udf: invalid file name %q", name)
	}
	return name, nil
}
