// ne_pmtiles.go — a PMTiles v3 writer (https://github.com/protomaps/PMTiles/blob/main/spec/v3/spec.md),
// native: 127-byte header, one gzip'd root directory (entries sorted by
// Hilbert tile id, run-length + offset deltas), gzip'd JSON metadata, tile
// data with identical tiles stored once. Tiles are handed in already gzip'd
// (tile compression = gzip), so an archive is a plain concatenation — the
// same bytes the MVT endpoint serves, no second compression pass.
//
// Root-only directories: the spec allows ~16 k entries before leaves are
// advisable; our bbox archives are capped at nePMTMaxTiles (ne_mvt_jobs.go)
// so a single root directory always suffices (readers handle any size).
package main

import (
	"bytes"
	"compress/gzip"
	"crypto/sha256"
	"encoding/binary"
	"io"
	"math"
	"sort"
)

// pmtTileID — the spec's Hilbert tile id for z/x/y.
func pmtTileID(z, x, y int) uint64 {
	var acc uint64
	for t := 0; t < z; t++ {
		acc += uint64(1) << (2 * uint(t))
	}
	n := 1 << uint(z)
	var d uint64
	rx, ry := 0, 0
	for s := n / 2; s > 0; s /= 2 {
		if x&s != 0 {
			rx = 1
		} else {
			rx = 0
		}
		if y&s != 0 {
			ry = 1
		} else {
			ry = 0
		}
		d += uint64(s) * uint64(s) * uint64((3*rx)^ry)
		// rotate
		if ry == 0 {
			if rx == 1 {
				x = s - 1 - x
				y = s - 1 - y
			}
			x, y = y, x
		}
	}
	return acc + d
}

type pmtEntry struct {
	id     uint64
	offset uint64
	length uint32
	run    uint32
}

// pmtWriter — collects tiles, writes the archive once.
type pmtWriter struct {
	entries []pmtEntry
	data    bytes.Buffer
	dedup   map[[32]byte]pmtEntry
	minZ    int
	maxZ    int
	box     [4]float64
	meta    []byte // JSON
	addr    uint64
}

func newPMTWriter(minZ, maxZ int, box [4]float64, meta []byte) *pmtWriter {
	return &pmtWriter{dedup: map[[32]byte]pmtEntry{}, minZ: minZ, maxZ: maxZ, box: box, meta: meta}
}

// add — one gzip'd tile; empty tiles are simply absent from the directory.
func (p *pmtWriter) add(z, x, y int, gz []byte) {
	if len(gz) == 0 {
		return
	}
	id := pmtTileID(z, x, y)
	h := sha256.Sum256(gz)
	p.addr++
	if e, ok := p.dedup[h]; ok {
		p.entries = append(p.entries, pmtEntry{id: id, offset: e.offset, length: e.length, run: 1})
		return
	}
	e := pmtEntry{id: id, offset: uint64(p.data.Len()), length: uint32(len(gz)), run: 1}
	p.data.Write(gz)
	p.dedup[h] = e
	p.entries = append(p.entries, e)
}

func pmtGzip(b []byte) []byte {
	var buf bytes.Buffer
	w, _ := gzip.NewWriterLevel(&buf, gzip.BestCompression)
	w.Write(b)
	w.Close()
	return buf.Bytes()
}

// dir — serialised directory (uncompressed), entries sorted by id.
func pmtDir(es []pmtEntry) []byte {
	sort.Slice(es, func(a, b int) bool { return es[a].id < es[b].id })
	// merge runs: consecutive ids, same offset+length (dedup'd identical tiles) → run_length
	var m []pmtEntry
	for _, e := range es {
		if n := len(m); n > 0 && m[n-1].offset == e.offset && m[n-1].length == e.length && m[n-1].id+uint64(m[n-1].run) == e.id {
			m[n-1].run++
			continue
		}
		m = append(m, e)
	}
	var b []byte
	b = pbVarint(b, uint64(len(m)))
	var last uint64
	for _, e := range m {
		b = pbVarint(b, e.id-last)
		last = e.id
	}
	for _, e := range m {
		b = pbVarint(b, uint64(e.run))
	}
	for _, e := range m {
		b = pbVarint(b, uint64(e.length))
	}
	for i, e := range m {
		if i > 0 && e.offset == m[i-1].offset+uint64(m[i-1].length) {
			b = pbVarint(b, 0)
		} else {
			b = pbVarint(b, e.offset+1)
		}
	}
	return b
}

// write — the whole archive to w. Returns bytes written.
func (p *pmtWriter) write(w io.Writer) (int64, error) {
	entries := append([]pmtEntry(nil), p.entries...)
	root := pmtGzip(pmtDir(entries))
	meta := pmtGzip(p.meta)
	const hdr = 127
	rootOff := uint64(hdr)
	metaOff := rootOff + uint64(len(root))
	leafOff := metaOff + uint64(len(meta))
	dataOff := leafOff
	h := make([]byte, hdr)
	copy(h, "PMTiles")
	h[7] = 3
	le := binary.LittleEndian
	le.PutUint64(h[8:], rootOff)
	le.PutUint64(h[16:], uint64(len(root)))
	le.PutUint64(h[24:], metaOff)
	le.PutUint64(h[32:], uint64(len(meta)))
	le.PutUint64(h[40:], leafOff)
	le.PutUint64(h[48:], 0)
	le.PutUint64(h[56:], dataOff)
	le.PutUint64(h[64:], uint64(p.data.Len()))
	le.PutUint64(h[72:], p.addr)                 // addressed tiles
	le.PutUint64(h[80:], uint64(len(p.entries))) // tile entries
	le.PutUint64(h[88:], uint64(len(p.dedup)))   // tile contents
	h[96] = 1                                    // clustered
	h[97] = 2                                    // internal compression gzip
	h[98] = 2                                    // tile compression gzip
	h[99] = 1                                    // tile type MVT
	h[100] = uint8(p.minZ)
	h[101] = uint8(p.maxZ)
	e7 := func(v float64) uint32 { return uint32(int32(math.Round(v * 1e7))) }
	le.PutUint32(h[102:], e7(p.box[0]))
	le.PutUint32(h[106:], e7(p.box[1]))
	le.PutUint32(h[110:], e7(p.box[2]))
	le.PutUint32(h[114:], e7(p.box[3]))
	h[118] = uint8(min(p.maxZ, p.minZ+2))
	le.PutUint32(h[119:], e7((p.box[0]+p.box[2])/2))
	le.PutUint32(h[123:], e7((p.box[1]+p.box[3])/2))
	var n int64
	for _, part := range [][]byte{h, root, meta, p.data.Bytes()} {
		k, err := w.Write(part)
		n += int64(k)
		if err != nil {
			return n, err
		}
	}
	return n, nil
}
