package main

// neChamfer — a two-pass 3-4 chamfer distance transform over d (0 = seed, large = unknown), shared by the
// veil's hole growth (neFltGrow). The state border itself is drawn client-side from the admin outline
// (ne_glyph_filt.go fltBorder): opaque page colour on the far side + the ink line, above the veil group.
// neChamfer — 3-4 chamfer distance transform in place: d holds 0 at the seeds and a large value elsewhere.
func neChamfer(d []int32, w, h int) {
	min := func(a, b int32) int32 {
		if a < b {
			return a
		}
		return b
	}
	for y := 0; y < h; y++ {
		for x := 0; x < w; x++ {
			v := d[y*w+x]
			if x > 0 {
				v = min(v, d[y*w+x-1]+3)
			}
			if y > 0 {
				v = min(v, d[(y-1)*w+x]+3)
				if x > 0 {
					v = min(v, d[(y-1)*w+x-1]+4)
				}
				if x+1 < w {
					v = min(v, d[(y-1)*w+x+1]+4)
				}
			}
			d[y*w+x] = v
		}
	}
	for y := h - 1; y >= 0; y-- {
		for x := w - 1; x >= 0; x-- {
			v := d[y*w+x]
			if x+1 < w {
				v = min(v, d[y*w+x+1]+3)
			}
			if y+1 < h {
				v = min(v, d[(y+1)*w+x]+3)
				if x+1 < w {
					v = min(v, d[(y+1)*w+x+1]+4)
				}
				if x > 0 {
					v = min(v, d[(y+1)*w+x-1]+4)
				}
			}
			d[y*w+x] = v
		}
	}
}
