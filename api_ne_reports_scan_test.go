package main

import (
	"testing"
	"time"
)

// A dirty reports memo is served as it is — a reader never walks the report
// files in its request (2026-11-22: the landing page waited ~55 s per read
// while a peer posted a report a minute).
func TestNEScanReportFilesServesStale(t *testing.T) {
	old := neDir
	neDir = t.TempDir()
	defer func() {
		neReportsWalkMu.Lock() // let a background walk finish before the dir goes
		neReportsWalkMu.Unlock()
		neDir = old
		neReportsScanMemo.Lock()
		neReportsScanMemo.data, neReportsScanMemo.kgs, neReportsScanMemo.dirty = nil, nil, false
		neReportsScanMemo.Unlock()
	}()
	neReportsScanMemo.Lock()
	neReportsScanMemo.data, neReportsScanMemo.kgs = nil, nil
	neReportsScanMemo.Unlock()

	first := neScanReportFiles() // nothing cached: walks
	if first == nil || first["scanned_at"] == nil {
		t.Fatalf("first scan: %v", first)
	}
	// dirty and a minute old: stale value, no walk, no background walk yet
	neReportsScanMemo.Lock()
	neReportsScanMemo.dirty = true
	neReportsScanMemo.at = time.Now().Add(-time.Minute)
	at := neReportsScanMemo.at
	neReportsScanMemo.Unlock()
	neReportsWalkMu.Lock() // a walk in the request would deadlock here
	done := make(chan struct{})
	go func() { neScanReportFiles(); neReportKGStats(); close(done) }()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("a reader waited for the reports walk")
	}
	neReportsWalkMu.Unlock()
	neReportsScanMemo.Lock()
	if neReportsScanMemo.walking || !neReportsScanMemo.at.Equal(at) {
		t.Fatal("a 1 min old dirty memo must not be rewalked by a reader")
	}
	// older than the gap: still answered at once, one background walk lands
	neReportsScanMemo.at = time.Now().Add(-2 * neReportsStaleGap)
	neReportsScanMemo.Unlock()
	neScanReportFiles()
	for i := 0; ; i++ {
		neReportsScanMemo.Lock()
		ok := !neReportsScanMemo.dirty && !neReportsScanMemo.walking && time.Since(neReportsScanMemo.at) < time.Minute
		neReportsScanMemo.Unlock()
		if ok {
			break
		}
		if i > 200 {
			t.Fatal("background walk did not land")
		}
		time.Sleep(10 * time.Millisecond)
	}
	// the exact value walks when dirty
	neReportsMarkDirtyForTest()
	time.Sleep(10 * time.Millisecond)
	neReportsScanMemo.Lock()
	neReportsScanMemo.at = time.Now().Add(-time.Minute)
	neReportsScanMemo.Unlock()
	neScanReportFilesFresh()
	neReportsScanMemo.Lock()
	defer neReportsScanMemo.Unlock()
	if neReportsScanMemo.dirty || time.Since(neReportsScanMemo.at) > 10*time.Second {
		t.Fatal("neScanReportFilesFresh did not walk a dirty memo")
	}
}

func neReportsMarkDirtyForTest() {
	neReportsScanMemo.Lock()
	neReportsScanMemo.dirty = true
	neReportsScanMemo.Unlock()
}
