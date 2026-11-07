package main

// ne_glyph_dbg.go — the `dbg=1` block of /api/v1/ne/glyph/cell: everything an
// operator needs to name one spot to the observed-layer peer (docs/ne-glyph.md
// §dbg where): the H3 ancestry res 7…12, the res-7 frame index, cell bboxes, the
// LOD tile keys and fidx bin of the point, and the raw observed row(s) decoded
// field by field with the layer header (source, epoch, built_at, digest) of the
// KG file they came from. Public tier, read-only, no SQL, no private prefix.

import (
	"math"
	"strconv"

	"github.com/uber/h3-go/v4"
)

func neGlyphDbgBBox(id uint64) []float64 {
	bd, err := h3.Cell(id).Boundary()
	if err != nil || len(bd) == 0 {
		return nil
	}
	w, s, e, n := 999.0, 999.0, -999.0, -999.0
	for _, v := range bd {
		w, e = math.Min(w, v.Lng), math.Max(e, v.Lng)
		s, n = math.Min(s, v.Lat), math.Max(n, v.Lat)
	}
	r := func(x float64) float64 { return math.Round(x*1e6) / 1e6 }
	return []float64{r(w), r(s), r(e), r(n)}
}

// neObsDecodeRow — every field of one obs row by name, walking the python
// struct format beside fields[] (same walk as neObsLayout; nil when the two
// disagree). Little-endian, as the writer packs.
func neObsDecodeRow(fields []string, record string, row []byte) map[string]interface{} {
	if len(fields) == 0 || record == "" {
		return nil
	}
	out := map[string]interface{}{}
	off, fi, i := 0, 0, 0
	if len(record) > 0 && (record[0] == '<' || record[0] == '>' || record[0] == '=' || record[0] == '!' || record[0] == '@') {
		i++
	}
	le := func(n int) uint64 {
		var v uint64
		for b := n - 1; b >= 0; b-- {
			v = v<<8 | uint64(row[off+b])
		}
		return v
	}
	for i < len(record) {
		n := 0
		for i < len(record) && record[i] >= '0' && record[i] <= '9' {
			n = n*10 + int(record[i]-'0')
			i++
		}
		if i >= len(record) {
			break
		}
		c := record[i]
		i++
		if n == 0 {
			n = 1
		}
		if c == 's' {
			if off+n > len(row) || fi >= len(fields) {
				return out
			}
			vals := make([]int, n)
			for k := 0; k < n; k++ {
				vals[k] = int(row[off+k])
			}
			out[fields[fi]] = vals
			off += n
			fi++
			continue
		}
		var sz int
		switch c {
		case 'Q', 'q', 'd':
			sz = 8
		case 'I', 'i', 'f', 'L', 'l':
			sz = 4
		case 'H', 'h':
			sz = 2
		case 'B', 'b', 'c', '?':
			sz = 1
		default:
			return out
		}
		for r := 0; r < n; r++ {
			if off+sz > len(row) || fi >= len(fields) {
				return out
			}
			u := le(sz)
			var v interface{}
			switch c {
			case 'Q':
				v = strconv.FormatUint(u, 16)
			case 'q':
				v = int64(u)
			case 'd':
				v = math.Float64frombits(u)
			case 'f':
				v = math.Float32frombits(uint32(u))
			case 'i', 'l':
				v = int32(uint32(u))
			case 'h':
				v = int16(uint16(u))
			case 'b':
				v = int8(uint8(u))
			default:
				v = u
			}
			out[fields[fi]] = v
			off += sz
			fi++
		}
	}
	return out
}

// neGlyphCellDbg — the block itself.
func neGlyphCellDbg(id uint64) map[string]interface{} {
	lon, lat := neCellCentre(id)
	d := map[string]interface{}{"cell": neCellHex(id), "lon": lon, "lat": lat, "bbox12": neGlyphDbgBBox(id)}
	par := map[string]string{}
	for res := neCellRes - 1; res >= 7; res-- {
		if p, err := h3.Cell(id).Parent(res); err == nil {
			par[strconv.Itoa(res)] = neCellHex(uint64(p))
		}
	}
	d["parents"] = par
	if p, err := h3.Cell(id).Parent(neGlyphRes); err == nil {
		fr := &neGlyphFrame
		if ix, ok := fr.index[uint64(p)]; ok {
			c := fr.cells[ix]
			d["r7"] = map[string]interface{}{"index": ix, "cell": neCellHex(uint64(p)), "x": c.x, "y": c.y, "lon": c.lon, "lat": c.lat, "bbox": neGlyphDbgBBox(uint64(p))}
		} else {
			d["r7"] = map[string]interface{}{"index": -1, "cell": neCellHex(uint64(p)), "bbox": neGlyphDbgBBox(uint64(p))}
		}
	}
	// glyph tiles: lod 0 = 0.025°, lod 1 = 0.01°, lod 2 = 0.1° (LOD[] in the page); the fidx bin is the lod-0 key
	tiles := map[string]string{}
	for lod, L := range []float64{0.025, 0.01, 0.1} {
		tiles[strconv.Itoa(lod)] = strconv.Itoa(lod) + "/" + strconv.Itoa(int(math.Floor(lon/L))) + "/" + strconv.Itoa(int(math.Floor(lat/L)))
	}
	d["tiles"] = tiles
	d["chunk9"] = neCellHex(h3Parent(id, neChunkRes))
	// the observed rows, one per KG file holding the cell
	var obs []map[string]interface{}
	if hits, err := neStore.cellRows(id, "obs"); err == nil {
		for _, h := range hits {
			k := neStore.get(h.KG)
			if k == nil {
				continue
			}
			o := map[string]interface{}{"kg": h.KG, "row_bytes": len(h.Row)}
			if l := k.Hdr.Layers["obs"]; l != nil {
				o["digest"] = l.Digest
				o["built_at"] = l.BuiltAt
				o["rows"] = l.Rows
			}
			rec := ""
			if hd := k.layerHeader("obs"); hd != nil {
				for _, key := range []string{"source", "epoch", "algo", "record", "as_of", "observer", "version"} {
					if v, ok := hd[key]; ok {
						o[key] = v
					}
				}
				rec, _ = hd["record"].(string)
			}
			if dec := neObsDecodeRow(neObsFields(k), rec, h.Row); dec != nil {
				o["row"] = dec
			}
			obs = append(obs, o)
		}
	}
	d["obs"] = obs
	if hits, err := neStore.cellRows(id, "lu"); err == nil {
		var lu []map[string]interface{}
		for _, h := range hits {
			lu = append(lu, map[string]interface{}{"kg": h.KG, "share": h.Share})
		}
		d["lu"] = lu
	}
	return d
}
