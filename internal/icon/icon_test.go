package icon

import (
	"bytes"
	"encoding/binary"
	"image/png"
	"testing"
)

func TestICOStructure(t *testing.T) {
	data := ICO(Running, 16, 32, 256)
	var hdr [3]uint16
	_ = binary.Read(bytes.NewReader(data), binary.LittleEndian, &hdr)
	if hdr != [3]uint16{0, 1, 3} {
		t.Fatalf("header = %v", hdr)
	}
	for i, size := range []int{16, 32, 256} {
		e := data[6+16*i:]
		n := binary.LittleEndian.Uint32(e[8:])
		off := binary.LittleEndian.Uint32(e[12:])
		if int(off+n) > len(data) {
			t.Fatalf("entry %d out of range", i)
		}
		blob := data[off : off+n]
		if size == 256 {
			if _, err := png.Decode(bytes.NewReader(blob)); err != nil {
				t.Fatal(err)
			}
			continue
		}
		mask := ((size + 31) / 32) * 4 * size
		if want := 40 + size*size*4 + mask; int(n) != want {
			t.Fatalf("dib %d size = %d, want %d", size, n, want)
		}
	}
}

func TestImageCorners(t *testing.T) {
	img := Image(32, Idle)
	if img.NRGBAAt(0, 0).A != 0 {
		t.Fatal("corner should be transparent")
	}
	if img.NRGBAAt(16, 16).A != 255 {
		t.Fatal("center should be opaque")
	}
}
