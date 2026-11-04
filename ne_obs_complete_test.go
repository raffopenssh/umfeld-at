package main

import "testing"

// A view_rekey padding row (all nodata) is blank; a real row with a height or a
// flight year is not, even when obs_lu is all zero.
func TestNEObsRowBlank(t *testing.T) {
	lay := neObsLayout(neObsFieldsV41, "<Q9sBBBBBBBBBBbBBHBBBBBbbBB")
	if lay == nil {
		t.Fatal("layout")
	}
	blank := make([]byte, lay.rowBytes)
	for i := lay.luOff + 9; i < len(blank); i++ {
		blank[i] = 255
	}
	blank[lay.flight] = 0
	if !neObsRowBlank(blank, lay) {
		t.Fatal("padding row not blank")
	}
	r := append([]byte(nil), blank...)
	r[lay.hMean] = 3
	if neObsRowBlank(r, lay) {
		t.Fatal("row with a height is not blank")
	}
	r = append([]byte(nil), blank...)
	r[lay.flight] = 1
	if neObsRowBlank(r, lay) {
		t.Fatal("row with a flight year is not blank")
	}
	r = append([]byte(nil), blank...)
	r[lay.luOff+4] = 255
	if neObsRowBlank(r, lay) {
		t.Fatal("row with a use is not blank")
	}
}
