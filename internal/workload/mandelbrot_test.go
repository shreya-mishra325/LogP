package workload

import (
	"encoding/binary"
	"math"
	"testing"
)

func TestPartitionAndReference(t *testing.T) {
	in := Input{Kernel: Kernel, Width: 17, Height: 19, MaxIter: 32, XMin: 3, XMax: 4, YMin: -1, YMax: 1, TileSize: 16}
	if err := in.Validate(); err != nil {
		t.Fatal(err)
	}
	rects := in.Partition()
	if len(rects) != 4 || rects[3] != (Rect{16, 16, 1, 3}) {
		t.Fatal(rects)
	}
	for _, r := range rects {
		data := Reference(Params{in, r})
		if !ValidTile(r, data, in.MaxIter) {
			t.Fatal("invalid tile")
		}
		for i := 0; i < len(data); i += 2 {
			if n := binary.LittleEndian.Uint16(data[i:]); n != 1 {
				t.Fatal(n)
			}
		}
	}
	in.XMax = math.MaxFloat64
	if in.Validate() == nil {
		t.Fatal("accepted overflowing coordinate mapping")
	}
}
