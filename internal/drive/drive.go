// Package drive talks to optical drives through the Linux CD-ROM ioctl
// interface. It needs no udev, no mount and no helper binaries, which keeps it
// working inside containers that only receive the block device.
package drive

import (
	"bytes"
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"syscall"
	"unicode/utf16"
)

// Status mirrors the kernel's CDS_* drive status values.
type Status int

const (
	NoInfo   Status = 0
	NoDisc   Status = 1
	TrayOpen Status = 2
	NotReady Status = 3
	DiscOK   Status = 4
)

func (s Status) String() string {
	switch s {
	case NoInfo:
		return "no-info"
	case NoDisc:
		return "no-disc"
	case TrayOpen:
		return "tray-open"
	case NotReady:
		return "not-ready"
	case DiscOK:
		return "disc-ok"
	}
	return fmt.Sprintf("status(%d)", int(s))
}

// Drive is the small surface the pipeline needs. The Linux implementation is
// the only real one; tests use fakes.
type Drive interface {
	Path() string
	Status() (Status, error)
	Eject() error
	CloseTray() error
	Lock(locked bool) error
	// Fingerprint returns a cheap, stable identifier for the inserted disc
	// together with its volume label (which may be empty).
	Fingerprint() (fp string, label string, err error)
}

const (
	ioctlDriveStatus = 0x5326 // CDROM_DRIVE_STATUS
	ioctlEject       = 0x5309 // CDROMEJECT
	ioctlCloseTray   = 0x5319 // CDROMCLOSETRAY
	ioctlLockDoor    = 0x5329 // CDROM_LOCKDOOR
	cdslCurrent      = 0x7fffffff
	sectorSize       = 2048
)

type linuxDrive struct{ path string }

// Open returns a Drive for a device node such as /dev/sr0. The device is not
// kept open between calls so the kernel can still handle the eject button.
func Open(path string) (Drive, error) {
	if _, err := os.Stat(path); err != nil {
		return nil, err
	}
	return &linuxDrive{path: path}, nil
}

// Discover lists /dev/sr* nodes in order.
func Discover() []string {
	matches, _ := filepath.Glob("/dev/sr[0-9]*")
	sort.Strings(matches)
	return matches
}

func (d *linuxDrive) Path() string { return d.path }

func (d *linuxDrive) fd() (int, error) {
	fd, err := syscall.Open(d.path, syscall.O_RDONLY|syscall.O_NONBLOCK|syscall.O_CLOEXEC, 0)
	if err != nil {
		return -1, fmt.Errorf("open %s: %w", d.path, err)
	}
	return fd, nil
}

func (d *linuxDrive) ioctl(req uintptr, arg uintptr) (int, error) {
	fd, err := d.fd()
	if err != nil {
		return 0, err
	}
	defer syscall.Close(fd)
	r, _, errno := syscall.Syscall(syscall.SYS_IOCTL, uintptr(fd), req, arg)
	if errno != 0 {
		return int(r), fmt.Errorf("ioctl %#x on %s: %w", req, d.path, errno)
	}
	return int(r), nil
}

func (d *linuxDrive) Status() (Status, error) {
	r, err := d.ioctl(ioctlDriveStatus, cdslCurrent)
	if err != nil {
		return NoInfo, err
	}
	return Status(r), nil
}

func (d *linuxDrive) Eject() error {
	_ = d.Lock(false)
	_, err := d.ioctl(ioctlEject, 0)
	return err
}

func (d *linuxDrive) CloseTray() error {
	_, err := d.ioctl(ioctlCloseTray, 0)
	return err
}

func (d *linuxDrive) Lock(locked bool) error {
	var v uintptr
	if locked {
		v = 1
	}
	_, err := d.ioctl(ioctlLockDoor, v)
	return err
}

func (d *linuxDrive) Fingerprint() (string, string, error) {
	f, err := os.Open(d.path)
	if err != nil {
		return "", "", err
	}
	defer f.Close()
	return FingerprintReader(f)
}

// FingerprintReader hashes the volume descriptor areas of an ISO9660/UDF image
// and extracts the volume label. It reads at most a few hundred KB.
func FingerprintReader(r io.ReaderAt) (string, string, error) {
	h := sha256.New()
	var label string

	// ISO9660 volume descriptors start at sector 16.
	iso := make([]byte, 32*sectorSize)
	n, err := r.ReadAt(iso, 16*sectorSize)
	if err != nil && !errors.Is(err, io.EOF) && n == 0 {
		return "", "", fmt.Errorf("read volume descriptors: %w", err)
	}
	iso = iso[:n]
	h.Write(iso)
	if len(iso) >= sectorSize && bytes.Equal(iso[1:6], []byte("CD001")) && iso[0] == 1 {
		label = cleanLabel(string(iso[40:72]))
	}

	// UDF anchor volume descriptor pointer lives at sector 256.
	avdp := make([]byte, sectorSize)
	if n, _ := r.ReadAt(avdp, 256*sectorSize); n == sectorSize {
		h.Write(avdp)
		if binary.LittleEndian.Uint16(avdp[0:2]) == 2 { // TAG_IDENT_AVDP
			vdsLen := binary.LittleEndian.Uint32(avdp[16:20])
			vdsLoc := binary.LittleEndian.Uint32(avdp[20:24])
			if vdsLen > 0 && vdsLen <= 64*sectorSize {
				vds := make([]byte, vdsLen)
				if n, _ := r.ReadAt(vds, int64(vdsLoc)*sectorSize); n > 0 {
					vds = vds[:n]
					h.Write(vds)
					if l := udfLabel(vds); l != "" && label == "" {
						label = l
					}
				}
			}
		}
	}
	sum := h.Sum(nil)
	return hex.EncodeToString(sum[:16]), label, nil
}

func udfLabel(vds []byte) string {
	for off := 0; off+sectorSize <= len(vds); off += sectorSize {
		tag := binary.LittleEndian.Uint16(vds[off : off+2])
		if tag == 1 { // TAG_IDENT_PVD
			return udfDString(vds[off+24 : off+56])
		}
		if tag == 8 { // terminating descriptor
			break
		}
	}
	return ""
}

// udfDString decodes an OSTA compressed unicode dstring.
func udfDString(b []byte) string {
	if len(b) < 2 {
		return ""
	}
	l := int(b[len(b)-1])
	if l <= 1 || l > len(b)-1 {
		return ""
	}
	data := b[1:l]
	switch b[0] {
	case 8:
		return cleanLabel(string(data))
	case 16:
		if len(data)%2 != 0 {
			data = data[:len(data)-1]
		}
		u := make([]uint16, len(data)/2)
		for i := range u {
			u[i] = binary.BigEndian.Uint16(data[2*i:])
		}
		return cleanLabel(string(utf16.Decode(u)))
	}
	return ""
}

func cleanLabel(s string) string {
	s = strings.TrimRight(s, " \x00")
	s = strings.Map(func(r rune) rune {
		if r < 0x20 || r == 0x7f {
			return -1
		}
		return r
	}, s)
	return strings.TrimSpace(s)
}
