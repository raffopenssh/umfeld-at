package main

// Rendered tiles on disk, behind the in-memory LRU. The LRU key of a tile
// carries the state of everything the picture reads (KG digests, filter
// index state, expression, day for the peer-fed views), so a stored tile is
// valid for as long as its key is asked for — across restarts and across
// readers. A miss in the 64 MB LRU used to mean a render behind the two-slot
// gate (p90 8–17 s on a phone screen of 100 tiles); now it is a ~0.1 ms read.
// A tile whose state moved is simply never asked for again and ages out.
//
// data/ne_tiles/<hh>/<sha256(key)[:32]> = "NTD1" + neGlyphTileJoin(meta, body).
// Bounded by bytes AND files (oldest use first); rebuildable — delete the
// directory freely. NE_TILE_DISK_MB=0 turns it off. Off under `go test`
// (fixtures reuse keys with other data). Size line: debug/inflight
// ne_glyph_tiles.disk.

import (
	"crypto/sha256"
	"encoding/hex"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

const (
	neGlyphTileDiskDir      = "data/ne_tiles"
	neGlyphTileDiskVer      = "NTD1"
	neGlyphTileDiskMaxFiles = 400_000
)

var (
	neGlyphTileDiskCap    = int64(4 << 30)
	neGlyphTileDiskOnce   sync.Once
	neGlyphTileDiskOn     bool
	neGlyphTileDiskMu     sync.Mutex
	neGlyphTileDiskSize   int64 = -1 // -1 = not walked yet
	neGlyphTileDiskFiles  int64
	neGlyphTileDiskSweeps int64
	neGlyphTileDiskHits   int64
	neGlyphTileDiskMiss   int64
	neGlyphTileDiskWrites int64
	neGlyphTileDiskQ      = make(chan [2]string, 256) // key hash, joined tile: one writer, never blocks a render
)

func neGlyphTileDiskInit() {
	neGlyphTileDiskOnce.Do(func() {
		if testing.Testing() {
			return
		}
		if v, err := strconv.Atoi(os.Getenv("NE_TILE_DISK_MB")); err == nil {
			neGlyphTileDiskCap = int64(v) << 20
		}
		if neGlyphTileDiskCap <= 0 {
			return
		}
		neGlyphTileDiskOn = true
		go func() {
			neGlyphTileDiskMu.Lock()
			neGlyphTileDiskSweep() // walks: sets size + files, trims to the cap
			neGlyphTileDiskMu.Unlock()
			for it := range neGlyphTileDiskQ {
				neGlyphTileDiskWrite(it[0], it[1])
			}
		}()
	})
}

// neGlyphTileHash — the key's name on disk; also the tile's ETag.
func neGlyphTileHash(key string) string {
	s := sha256.Sum256([]byte(key))
	return hex.EncodeToString(s[:16])
}

func neGlyphTileDiskPath(h string) string { return filepath.Join(neGlyphTileDiskDir, h[:2], h) }

// neGlyphTileGet — LRU, then disk (a disk hit is put back into the LRU).
func neGlyphTileGet(key string) ([]byte, bool) {
	if b, ok := neGlyphTileLRU.get(key); ok {
		return b, true
	}
	neGlyphTileDiskInit()
	if !neGlyphTileDiskOn {
		return nil, false
	}
	p := neGlyphTileDiskPath(neGlyphTileHash(key))
	b, err := os.ReadFile(p)
	if err != nil || len(b) < 4 || string(b[:4]) != neGlyphTileDiskVer {
		atomic.AddInt64(&neGlyphTileDiskMiss, 1)
		return nil, false
	}
	atomic.AddInt64(&neGlyphTileDiskHits, 1)
	if fi, err := os.Stat(p); err == nil && time.Since(fi.ModTime()) > time.Hour {
		now := time.Now()
		_ = os.Chtimes(p, now, now) // mtime = last use (hour-grained), for the sweep
	}
	b = b[4:]
	neGlyphTileLRU.put(key, b)
	return b, true
}

// neGlyphTilePut — a complete tile: LRU + queued for disk (dropped when the
// writer is behind; the next render of it queues again).
func neGlyphTilePut(key string, joined []byte) {
	neGlyphTileLRU.put(key, joined)
	neGlyphTileDiskInit()
	if !neGlyphTileDiskOn {
		return
	}
	select {
	case neGlyphTileDiskQ <- [2]string{neGlyphTileHash(key), string(joined)}:
	default:
	}
}

func neGlyphTileDiskWrite(h, joined string) {
	p := neGlyphTileDiskPath(h)
	if _, err := os.Stat(p); err == nil {
		return
	}
	if os.MkdirAll(filepath.Dir(p), 0o755) != nil {
		return
	}
	tmp := p + ".tmp"
	if err := os.WriteFile(tmp, []byte(neGlyphTileDiskVer+joined), 0o644); err != nil {
		_ = os.Remove(tmp)
		return
	}
	if os.Rename(tmp, p) != nil {
		_ = os.Remove(tmp)
		return
	}
	atomic.AddInt64(&neGlyphTileDiskWrites, 1)
	neGlyphTileDiskMu.Lock()
	defer neGlyphTileDiskMu.Unlock()
	neGlyphTileDiskSize += int64(len(joined) + 4)
	neGlyphTileDiskFiles++
	if neGlyphTileDiskSize > neGlyphTileDiskCap || neGlyphTileDiskFiles > neGlyphTileDiskMaxFiles {
		neGlyphTileDiskSweep()
	}
}

// neGlyphTileDiskSweep — under the lock: walk, then drop the least recently
// used files down to 85 % of both bounds.
func neGlyphTileDiskSweep() {
	type f struct {
		p  string
		sz int64
		t  int64
	}
	var fs []f
	sz := int64(0)
	_ = filepath.Walk(neGlyphTileDiskDir, func(p string, fi os.FileInfo, err error) error {
		if err == nil && !fi.IsDir() {
			fs = append(fs, f{p, fi.Size(), fi.ModTime().Unix()})
			sz += fi.Size()
		}
		return nil
	})
	n := int64(len(fs))
	if sz > neGlyphTileDiskCap || n > neGlyphTileDiskMaxFiles {
		sort.Slice(fs, func(a, b int) bool { return fs[a].t < fs[b].t })
		for _, x := range fs {
			if sz <= neGlyphTileDiskCap*85/100 && n <= neGlyphTileDiskMaxFiles*85/100 {
				break
			}
			if os.Remove(x.p) == nil {
				sz -= x.sz
				n--
			}
		}
		neGlyphTileDiskSweeps++
	}
	neGlyphTileDiskSize, neGlyphTileDiskFiles = sz, n
}

func neGlyphTileDiskStats() map[string]interface{} {
	neGlyphTileDiskMu.Lock()
	sz, n, sw := neGlyphTileDiskSize, neGlyphTileDiskFiles, neGlyphTileDiskSweeps
	neGlyphTileDiskMu.Unlock()
	return map[string]interface{}{
		"on": neGlyphTileDiskOn, "bytes": sz, "files": n, "cap": neGlyphTileDiskCap, "max_files": neGlyphTileDiskMaxFiles,
		"hits": atomic.LoadInt64(&neGlyphTileDiskHits), "misses": atomic.LoadInt64(&neGlyphTileDiskMiss),
		"writes": atomic.LoadInt64(&neGlyphTileDiskWrites), "sweeps": sw, "queue": len(neGlyphTileDiskQ),
	}
}
