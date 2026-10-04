package drive

import (
	"bytes"
	"encoding/binary"
	"testing"
	"time"
)

type memImage []byte

func (m memImage) ReadAt(p []byte, off int64) (int, error) {
	if off >= int64(len(m)) {
		return 0, errEOF
	}
	n := copy(p, m[off:])
	if n < len(p) {
		return n, errEOF
	}
	return n, nil
}

func isoImage(label string) memImage {
	img := make([]byte, 300*sectorSize)
	pvd := img[16*sectorSize:]
	pvd[0] = 1
	copy(pvd[1:], "CD001")
	pvd[6] = 1
	copy(pvd[40:72], []byte(label + "                                ")[:32])
	term := img[17*sectorSize:]
	term[0] = 255
	copy(term[1:], "CD001")
	return img
}

func udfImage(label string) memImage {
	img := make([]byte, 300*sectorSize)
	avdp := img[256*sectorSize:]
	binary.LittleEndian.PutUint16(avdp[0:], 2)
	binary.LittleEndian.PutUint32(avdp[16:], sectorSize*2)
	binary.LittleEndian.PutUint32(avdp[20:], 32)
	pvd := img[32*sectorSize:]
	binary.LittleEndian.PutUint16(pvd[0:], 1)
	d := pvd[24:56]
	d[0] = 8
	copy(d[1:], label)
	d[31] = byte(len(label) + 1)
	return img
}

func TestFingerprintISO(t *testing.T) {
	fp, label, err := FingerprintReader(isoImage("THE_MATRIX"))
	if err != nil {
		t.Fatal(err)
	}
	if label != "THE_MATRIX" {
		t.Fatalf("label = %q", label)
	}
	fp2, _, _ := FingerprintReader(isoImage("OTHER_DISC"))
	if fp == fp2 || len(fp) != 32 {
		t.Fatalf("fingerprints should differ: %s %s", fp, fp2)
	}
	fp3, _, _ := FingerprintReader(isoImage("THE_MATRIX"))
	if fp != fp3 {
		t.Fatal("fingerprint not stable")
	}
}

func TestFingerprintUDF(t *testing.T) {
	_, label, err := FingerprintReader(udfImage("FRIENDS_S1_D2"))
	if err != nil {
		t.Fatal(err)
	}
	if label != "FRIENDS_S1_D2" {
		t.Fatalf("label = %q", label)
	}
}

func TestUDFDStringUTF16(t *testing.T) {
	b := make([]byte, 32)
	b[0] = 16
	s := "Ünïcode"
	i := 1
	for _, r := range s {
		binary.BigEndian.PutUint16(b[i:], uint16(r))
		i += 2
	}
	b[31] = byte(i)
	if got := udfDString(b); got != s {
		t.Fatalf("got %q", got)
	}
}

func TestEmptyImage(t *testing.T) {
	_, label, err := FingerprintReader(memImage(bytes.Repeat([]byte{0}, 20*sectorSize)))
	if err != nil || label != "" {
		t.Fatalf("got %q %v", label, err)
	}
}

func TestConfirmEjected(t *testing.T) {
	seq := func(sts ...Status) func() (Status, error) {
		i := 0
		return func() (Status, error) {
			st := sts[min(i, len(sts)-1)]
			i++
			return st, nil
		}
	}
	cases := []struct {
		name    string
		status  func() (Status, error)
		wantErr bool
	}{
		{"opens after a moment", seq(DiscOK, NotReady, TrayOpen), false},
		{"slot loader reports no disc", seq(DiscOK, NoDisc), false},
		{"refused while mounted", seq(DiscOK), true},
		{"drive cannot report", seq(NoInfo), false},
	}
	for _, c := range cases {
		err := confirmEjected(c.status, 20*time.Millisecond, time.Millisecond)
		if (err != nil) != c.wantErr {
			t.Errorf("%s: err = %v, wantErr %v", c.name, err, c.wantErr)
		}
	}
}
