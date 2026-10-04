package udf

import (
	"encoding/binary"
	"unicode/utf16"
)

// A minimal UDF image writer covering what the reader needs. mkudffs only
// writes UDF 2.50 as VAT-based BD-R, never with the metadata partition pressed
// Blu-rays use, and populating its images needs a root loop mount, so most
// test images are built here.

type tnode struct {
	name string
	size int64
	kids []*tnode // non-nil for directories
	wide bool     // 16-bit CS0 name
	loop bool     // directory entry pointing back at the root
}

func dir(name string, kids ...*tnode) *tnode {
	if kids == nil {
		kids = []*tnode{}
	}
	return &tnode{name: name, kids: kids}
}

func file(name string, size int64) *tnode { return &tnode{name: name, size: size} }

type buildOpts struct {
	metadata   bool // UDF 2.50 layout: metadata partition, EFEs
	dirAD      int  // adShort, adLong or adEmbedded (embedded falls back to short when too big)
	aed        bool // route directory allocation descriptors through an allocation extent descriptor
	badMainMD  bool // corrupt the main metadata file entry so the mirror must be used
	badMainVDS bool // corrupt the main volume descriptor sequence
	indirect   bool // reach the root through an indirect entry
}

const (
	imgSectors = 640
	partStart  = 272
	partLen    = imgSectors - partStart
	mainVDS    = 32
	resVDS     = 48
)

// Metadata file runs, in physical partition blocks; deliberately split so
// directories straddle the gap.
var metaRuns = []run{{4, 30}, {100, 200}}

type builder struct {
	img     []byte
	opts    buildOpts
	next    uint32 // next free block in the filesystem's partition
	maxSec  int
	rootBlk uint32
	serial  uint16
}

func buildImage(root *tnode, o buildOpts) (img []byte, maxSector int) {
	b := &builder{img: make([]byte, imgSectors*sectorSize), opts: o}
	b.writeVDS(mainVDS)
	b.writeVDS(resVDS)
	if o.badMainVDS {
		b.img[mainVDS*sectorSize+4] ^= 0xff
		b.img[(mainVDS+1)*sectorSize+4] ^= 0xff
	}
	avdp := b.sectorBuf(256)
	binary.LittleEndian.PutUint32(avdp[16:], 16*sectorSize)
	binary.LittleEndian.PutUint32(avdp[20:], mainVDS)
	binary.LittleEndian.PutUint32(avdp[24:], 16*sectorSize)
	binary.LittleEndian.PutUint32(avdp[28:], resVDS)
	b.tag(avdp, tagAVDP, 256)

	if o.metadata {
		// Metadata file and its mirror at physical blocks 0 and 1.
		var ads []byte
		total := uint32(0)
		for _, r := range metaRuns {
			ads = binary.LittleEndian.AppendUint32(ads, r.count*sectorSize)
			ads = binary.LittleEndian.AppendUint32(ads, r.start)
			total += r.count
		}
		for i, ft := range []byte{250, 251} {
			blk := b.physBuf(uint32(i))
			b.fileEntry(blk, uint32(i), ft, uint64(total)*sectorSize, adShort, ads)
		}
		if o.badMainMD {
			b.physBuf(0)[4] ^= 0xff
		}
	}

	fsd := b.alloc()
	if o.indirect {
		ind := b.alloc()
		b.rootBlk = b.alloc()
		buf := b.blockBuf(ind)
		b.putLongAD(buf[36:], sectorSize, b.rootBlk)
		b.tag(buf, tagIndirect, ind)
		buf = b.blockBuf(fsd)
		b.putLongAD(buf[400:], sectorSize, ind)
		b.tag(buf, tagFileSet, fsd)
		b.writeNode(root, b.rootBlk, b.rootBlk)
	} else {
		b.rootBlk = b.alloc()
		buf := b.blockBuf(fsd)
		b.putLongAD(buf[400:], sectorSize, b.rootBlk)
		b.tag(buf, tagFileSet, fsd)
		b.writeNode(root, b.rootBlk, b.rootBlk)
	}
	return b.img, b.maxSec
}

func (b *builder) fsRef() uint16 {
	if b.opts.metadata {
		return 1
	}
	return 0
}

func (b *builder) alloc() uint32 { n := b.next; b.next++; return n }

func (b *builder) sectorBuf(s int) []byte {
	if s > b.maxSec {
		b.maxSec = s
	}
	return b.img[s*sectorSize : (s+1)*sectorSize]
}

func (b *builder) physBuf(blk uint32) []byte { return b.sectorBuf(partStart + int(blk)) }

// blockBuf returns a block of the partition the filesystem lives in.
func (b *builder) blockBuf(blk uint32) []byte {
	if !b.opts.metadata {
		return b.physBuf(blk)
	}
	for _, r := range metaRuns {
		if blk < r.count {
			return b.physBuf(r.start + blk)
		}
		blk -= r.count
	}
	panic("metadata partition full")
}

func (b *builder) tag(buf []byte, id uint16, loc uint32) {
	binary.LittleEndian.PutUint16(buf[0:], id)
	binary.LittleEndian.PutUint16(buf[2:], 2)
	b.serial++
	binary.LittleEndian.PutUint16(buf[6:], b.serial)
	binary.LittleEndian.PutUint32(buf[12:], loc)
	var sum byte
	for i := 0; i < 16; i++ {
		if i != 4 {
			sum += buf[i]
		}
	}
	buf[4] = sum
}

func (b *builder) putLongAD(buf []byte, length, blk uint32) {
	binary.LittleEndian.PutUint32(buf[0:], length)
	binary.LittleEndian.PutUint32(buf[4:], blk)
	binary.LittleEndian.PutUint16(buf[8:], b.fsRef())
}

func (b *builder) writeVDS(at int) {
	pd := b.sectorBuf(at)
	binary.LittleEndian.PutUint32(pd[16:], 1)
	binary.LittleEndian.PutUint16(pd[22:], 0)
	copy(pd[25:], "+NSR02")
	binary.LittleEndian.PutUint32(pd[184:], 1)
	binary.LittleEndian.PutUint32(pd[188:], partStart)
	binary.LittleEndian.PutUint32(pd[192:], partLen)
	b.tag(pd, tagPartition, uint32(at))

	lvd := b.sectorBuf(at + 1)
	binary.LittleEndian.PutUint32(lvd[16:], 2)
	binary.LittleEndian.PutUint32(lvd[212:], sectorSize)
	b.putLongAD(lvd[248:], sectorSize, 0)
	maps := lvd[440:]
	maps[0], maps[1] = 1, 6
	binary.LittleEndian.PutUint16(maps[2:], 1)
	n, size := uint32(1), uint32(6)
	if b.opts.metadata {
		m := maps[6:]
		m[0], m[1] = 2, 64
		copy(m[5:], "*UDF Metadata Partition")
		binary.LittleEndian.PutUint16(m[36:], 1)
		binary.LittleEndian.PutUint16(m[38:], 0)
		binary.LittleEndian.PutUint32(m[40:], 0)
		binary.LittleEndian.PutUint32(m[44:], 1)
		binary.LittleEndian.PutUint32(m[48:], 0xffffffff)
		n, size = 2, 70
	}
	binary.LittleEndian.PutUint32(lvd[264:], size)
	binary.LittleEndian.PutUint32(lvd[268:], n)
	b.tag(lvd, tagLogicalVol, uint32(at+1))

	b.tag(b.sectorBuf(at+2), tagTerminating, uint32(at+2))
}

// fileEntry fills buf with a File Entry, or an Extended one in the 2.50 layout.
func (b *builder) fileEntry(buf []byte, loc uint32, fileType byte, size uint64, alloc int, ads []byte) {
	id, hdr, adLenOff := uint16(tagFileEntry), 176, 172
	if b.opts.metadata {
		id, hdr, adLenOff = tagExtFileEnt, 216, 212
		binary.LittleEndian.PutUint64(buf[64:], size)
	}
	binary.LittleEndian.PutUint16(buf[20:], 4)
	buf[27] = fileType
	binary.LittleEndian.PutUint16(buf[34:], uint16(alloc))
	binary.LittleEndian.PutUint16(buf[48:], 1)
	binary.LittleEndian.PutUint64(buf[56:], size)
	binary.LittleEndian.PutUint32(buf[adLenOff:], uint32(len(ads)))
	copy(buf[hdr:], ads)
	b.tag(buf, id, loc)
}

func encodeName(n *tnode) []byte {
	if n.wide {
		out := []byte{16}
		for _, u := range utf16.Encode([]rune(n.name)) {
			out = append(out, byte(u>>8), byte(u))
		}
		return out
	}
	out := []byte{8}
	for _, r := range n.name {
		out = append(out, byte(r))
	}
	return out
}

func (b *builder) fid(chars byte, name []byte, icb uint32, implUse int) []byte {
	l := 38 + implUse + len(name)
	buf := make([]byte, (l+3)&^3)
	buf[18] = chars
	buf[19] = byte(len(name))
	b.putLongAD(buf[20:], sectorSize, icb)
	binary.LittleEndian.PutUint16(buf[36:], uint16(implUse))
	copy(buf[38+implUse:], name)
	b.tag(buf, tagFileID, 0)
	return buf
}

func (b *builder) writeNode(n *tnode, blk, parent uint32) {
	buf := b.blockBuf(blk)
	if n.kids == nil {
		b.fileEntry(buf, blk, fileTypeRegular, uint64(n.size), adShort, nil)
		return
	}
	data := b.fid(fidParent|2, nil, parent, 0)
	// A deleted entry pointing nowhere must be skipped without being read.
	data = append(data, b.fid(fidDeleted, []byte{8, 'x'}, 0xfffffff0, 0)...)
	for i, k := range n.kids {
		if k.loop {
			data = append(data, b.fid(2, encodeName(k), b.rootBlk, 0)...)
			continue
		}
		kb := b.alloc()
		b.writeNode(k, kb, blk)
		data = append(data, b.fid(0, encodeName(k), kb, 2*(i%2))...)
	}

	hdr := 176
	if b.opts.metadata {
		hdr = 216
	}
	if b.opts.dirAD == adEmbedded && hdr+len(data) <= sectorSize {
		b.fileEntry(buf, blk, fileTypeDir, uint64(len(data)), adEmbedded, data)
		return
	}
	nblk := (len(data) + sectorSize - 1) / sectorSize
	first := b.next
	for i := 0; i < nblk; i++ {
		copy(b.blockBuf(b.alloc()), data[i*sectorSize:min(len(data), (i+1)*sectorSize)])
	}
	var ads []byte
	alloc := adShort
	if b.opts.dirAD == adLong {
		// One descriptor per block exercises multi-extent reads.
		alloc = adLong
		for i := 0; i < nblk; i++ {
			ad := make([]byte, 16)
			b.putLongAD(ad, uint32(min(sectorSize, len(data)-i*sectorSize)), first+uint32(i))
			ads = append(ads, ad...)
		}
	} else {
		ads = binary.LittleEndian.AppendUint32(nil, uint32(len(data)))
		ads = binary.LittleEndian.AppendUint32(ads, first)
	}
	if b.opts.aed {
		aedBlk := b.alloc()
		aed := b.blockBuf(aedBlk)
		binary.LittleEndian.PutUint32(aed[20:], uint32(len(ads)))
		copy(aed[24:], ads)
		b.tag(aed, tagAllocExtent, aedBlk)
		if alloc == adLong {
			ads = make([]byte, 16)
			b.putLongAD(ads, 3<<30|sectorSize, aedBlk)
		} else {
			ads = binary.LittleEndian.AppendUint32(nil, 3<<30|sectorSize)
			ads = binary.LittleEndian.AppendUint32(ads, aedBlk)
		}
	}
	b.fileEntry(buf, blk, fileTypeDir, uint64(len(data)), alloc, ads)
}
