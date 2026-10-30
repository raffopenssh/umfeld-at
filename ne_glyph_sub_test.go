package main

import (
	"encoding/binary"
	"testing"
)

// TestNESubBlockSplit — the peer's ?block=1 layout: u32 n_bins=16, then
// 16 × {u32 len, exact per-bin body}, k = i*4+j.
func TestNESubBlockSplit(t *testing.T) {
	le := binary.LittleEndian
	h3bin := func(cells ...uint64) []byte {
		b := make([]byte, 4, 4+len(cells)*14)
		le.PutUint32(b, uint32(len(cells)))
		for _, c := range cells {
			b = le.AppendUint64(b, c)
		}
		for col := 0; col < 6; col++ {
			for range cells {
				b = append(b, byte(col+1))
			}
		}
		return b
	}
	empty := h3bin()
	full := h3bin(0x8c1f0000000001, 0x8c1f0000000002)
	var blk []byte
	blk = le.AppendUint32(blk, 16)
	for k := 0; k < 16; k++ {
		body := empty
		if k == 5 {
			body = full
		}
		blk = le.AppendUint32(blk, uint32(len(body)))
		blk = append(blk, body...)
	}
	raws, err := neSubBlockSplit(false, blk)
	if err != nil {
		t.Fatal(err)
	}
	if len(raws) != 16 || len(raws[5].Body) != len(full) || len(raws[0].Body) != 4 {
		t.Fatalf("bad split: %d bins, bin5=%d bin0=%d", len(raws), len(raws[5].Body), len(raws[0].Body))
	}
	bin, err := neSubParse(raws[5].Body)
	if err != nil || bin.N != 2 || bin.Cov[1] != 2 {
		t.Fatalf("bin 5 parse: %v %+v", err, bin)
	}
	// bin index k = i*4 + j → (I*4 + k/4, J*4 + k%4)
	if i, j := 4*3+5/4, 4*7+5%4; i != 13 || j != 29 {
		t.Fatalf("k→(i,j) %d %d", i, j)
	}
	// corrupt: trailing bytes, wrong n_bins, short
	if _, err := neSubBlockSplit(false, append(blk, 0)); err == nil {
		t.Fatal("trailing bytes accepted")
	}
	bad := append([]byte{}, blk...)
	le.PutUint32(bad, 15)
	if _, err := neSubBlockSplit(false, bad); err == nil {
		t.Fatal("n_bins 15 accepted")
	}
	if _, err := neSubBlockSplit(false, blk[:len(blk)-3]); err == nil {
		t.Fatal("short block accepted")
	}
	// poly: an empty IVP1 record per bin
	var pblk []byte
	pblk = le.AppendUint32(pblk, 16)
	for k := 0; k < 16; k++ {
		pblk = le.AppendUint32(pblk, 8)
		pblk = append(pblk, "IVP1"...)
		pblk = le.AppendUint32(pblk, 0)
	}
	if raws, err := neSubBlockSplit(true, pblk); err != nil || len(raws) != 16 {
		t.Fatalf("poly split: %v", err)
	}
	if floorDiv4(-1) != -1 || floorDiv4(7) != 1 || floorDiv4(-4) != -1 {
		t.Fatal("floorDiv4")
	}
}
