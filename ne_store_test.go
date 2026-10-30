package main

import (
	"bytes"
	"os"
	"path/filepath"
	"sync"
	"testing"
)

// neTestStore copies the real 63330 inputs (lu + bev) into a temp store dir
// and converts them. Skips when the inputs are not on this machine.
// neLiveStore — the production store under data/ne_cells, loaded once per test
// process into a private neStoreT (the global neStore stays untouched). On the
// live box every input has been converted to .nex, so fixtures are
// materialised back from it (private tier = verbatim header; the digest is
// over the rows, so the fixture is byte-equivalent to the original input).
var (
	neLiveOnce  sync.Once
	neLiveStore *neStoreT
)

func neFixtureInput(kg, layer string) []byte {
	kind := map[string]string{"lu": "nec", "bev": "bev.nec"}[layer]
	for _, p := range []string{"data/ne_cells/" + kg + "." + kind, "testdata/" + kg + "." + kind} {
		if b, err := os.ReadFile(p); err == nil {
			return b
		}
	}
	neLiveOnce.Do(func() {
		if _, err := os.Stat("data/ne_cells"); err != nil {
			return
		}
		saved := neStore
		neResetStoreForTest()
		neStore.load()
		neLiveStore, neStore = neStore, saved
	})
	if neLiveStore == nil {
		return nil
	}
	k := neLiveStore.get(kg)
	if k == nil || k.Hdr.Layers[layer] == nil {
		return nil
	}
	b, err := neLiveStore.materialise(k, []string{layer}, false)
	if err != nil {
		return nil
	}
	return b
}

func neTestStore(t *testing.T) (dir string, cleanup func()) {
	t.Helper()
	var src []byte
	src = neFixtureInput("63330", "lu")
	if len(src) == 0 {
		t.Skip("63330.nec not available")
	}
	dir = t.TempDir()
	os.MkdirAll(filepath.Join(dir, "dict"), 0o755)
	if d, err := os.ReadFile("data/ne_cells/dict/lu-1.zd"); err == nil {
		os.WriteFile(filepath.Join(dir, "dict", "lu-1.zd"), d, 0o644)
	}
	os.WriteFile(filepath.Join(dir, "63330.nec"), src, 0o644)
	if b := neFixtureInput("63330", "bev"); len(b) > 0 {
		os.WriteFile(filepath.Join(dir, "63330.bev.nec"), b, 0o644)
	}
	old := neDirOverride(dir)
	saved := neStore
	neResetStoreForTest()
	neStore.load()
	return dir, func() { neDirOverride(old); neStore = saved }
}

func TestNEStoreRoundTrip(t *testing.T) {
	dir, cleanup := neTestStore(t)
	defer cleanup()
	src, _ := os.ReadFile(filepath.Join(dir, "63330.nec"))
	secs, _ := parseNESections(src)
	srcRows, _ := neInflate(secs[0].Body)
	if err := neStore.convert("63330"); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(dir, "63330.nec")); err == nil {
		t.Fatal("input must be deleted after a verified conversion")
	}
	k := neStore.get("63330")
	if k == nil || k.Hdr.Layers["lu"] == nil {
		t.Fatal("KG not installed")
	}
	nex, _ := os.Stat(filepath.Join(dir, "63330.nex"))
	t.Logf("nec %d B → nex %d B (%.2f×), chunks %d, dict %q", len(src), nex.Size(), float64(nex.Size())/float64(len(src)), len(k.Hdr.Dir), k.Hdr.Layers["lu"].Dict)
	if k.Hdr.Layers["bev"] == nil && nex.Size() > int64(len(src)) {
		t.Errorf("nex larger than nec")
	}
	// materialised container: same digest, same header bytes on the private tier
	out, err := neStore.materialise(k, []string{"lu"}, false)
	if err != nil {
		t.Fatal(err)
	}
	ms, err := parseNESections(out)
	if err != nil || len(ms) != 1 || ms[0].Header["digest"] != secs[0].Header["digest"] {
		t.Fatalf("materialised digest mismatch: %v", err)
	}
	mrows, _ := neInflate(ms[0].Body)
	if !bytes.Equal(mrows, srcRows) {
		t.Fatal("materialised record block differs")
	}
	hl := int(uint32(src[5]) | uint32(src[6])<<8 | uint32(src[7])<<16 | uint32(src[8])<<24)
	if !bytes.Equal(out[9:9+hl], src[9:9+hl]) {
		t.Fatal("private header must be verbatim")
	}
	// bin rows for an id set byte-equal the source rows
	ids := []uint64{neRowID(srcRows, 0, 30), neRowID(srcRows, 1000, 30), neRowID(srcRows, 52000, 30)}
	for _, id := range ids {
		row, _, err := neStore.declaredRow(id, false)
		if err != nil || row == nil || !bytes.Equal(row, neFindRow(srcRows[:52476*30], 30, id)) {
			t.Fatalf("row %x: %v", id, err)
		}
	}
	// restart: reload from disk (header cache) and verify again
	neResetStoreForTest()
	neStore.load()
	k2 := neStore.get("63330")
	if k2 == nil || neStore.verify(k2) != nil {
		t.Fatal("reload/verify failed")
	}
	// public header is rewritten, digest unchanged
	pub, err := neStore.materialise(k2, []string{"lu"}, true)
	if err != nil {
		t.Fatal(err)
	}
	ps, err := parseNESections(pub)
	if err != nil || ps[0].Header["digest"] != secs[0].Header["digest"] || ps[0].Header["notice"] == secs[0].Header["notice"] {
		t.Fatalf("public container: %v", err)
	}
}
