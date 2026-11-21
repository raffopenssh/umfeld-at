// tree_frames.go — POSITION FRAMES of the tree layer (observed-layer peer
// contract #position-frames, 2026-11-21).
//
// The peer corrects the tree positions of a KG after the fact (a constant
// shift in EPSG:3035). A copy of its trees therefore lives in a frame: the
// token of its /trees/bbox answer ("frames": {"<kg>": "p-5+5"}, absent = "p0",
// never moved). Every copy we hold or hand out keeps that token and the time it
// was downloaded, and every report made from a copy says which frame it was in:
//
//	browser tile      e.fr / e.at            → "frame" on the report (trFrame, ne_glyph_treemerged.go)
//	piece cache       row byte 12 + header   → "frame" on every exported point, "as_of"/"timeStamp" on the file
//	mailed file       the row's frame field  → "frame"; a file without one is asked first (treeFrameAsk)
//
// The peer answers a report from an older copy with frame{moved_m}: our copy
// of that KG is stale. Here that means the day-keyed tree caches — their keys
// carry treeFrameGen, bumped when the peer's frames_rev changes (asked at most
// every treeFrameEvery while trees are being served, at once after a moved_m).
// Nothing is polled when nobody asks for trees.
package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"math"
	"net/http"
	"regexp"
	"sync"
	"sync/atomic"
	"time"
)

const (
	treeFrameEvery    = 10 * time.Minute
	treeFrameEveryHot = 30 * time.Second // after a moved_m answer
	treeFramePoints   = 20000            // the peer's limit per question
	treeFrameZero     = "p0"             // the frame of a KG that never moved
)

// treeFrameRe — a frame token as the peer writes it ("p0", "p-5+5").
var treeFrameRe = regexp.MustCompile(`^p[0-9+\-]{1,14}$`)

// treeFrameToken — t when it is a token, else "".
func treeFrameToken(t string) string {
	if treeFrameRe.MatchString(t) {
		return t
	}
	return ""
}

var treeFrameGen atomic.Int64 // part of every tree cache key

var treeFrameState struct {
	mu     sync.Mutex
	at     time.Time
	busy   bool
	seen   bool
	rev    string // the peer's frames_rev, raw JSON
	moved  int    // KGs it lists as moved
	checks int64
	bumps  int64
	hints  int64 // moved_m answers seen
}

// treeFrameURL — the peer's frame endpoint; a var so tests can point it at a stub.
var treeFrameURL = func() string {
	if a := peers().ObsAPI; a != "" {
		return a + "/api/v1/trees/frame"
	}
	return ""
}

// treeFrameCheck — ask the peer for frames_rev when the last answer is old
// (hot: a report was just answered with moved_m). Never blocks the caller.
func treeFrameCheck(hot bool) {
	s := &treeFrameState
	every := treeFrameEvery
	if hot {
		every = treeFrameEveryHot
	}
	s.mu.Lock()
	if hot {
		s.hints++
	}
	if s.busy || time.Since(s.at) < every || treeFrameURL() == "" {
		s.mu.Unlock()
		return
	}
	s.busy = true
	s.mu.Unlock()
	go func() {
		rev, moved, ok := "", 0, false
		ctx, cancel := context.WithTimeout(context.Background(), 8*time.Second)
		defer cancel()
		if req, err := http.NewRequestWithContext(ctx, http.MethodGet, treeFrameURL(), nil); err == nil {
			if resp, err := http.DefaultClient.Do(req); err == nil {
				var d struct {
					KGs map[string]json.RawMessage `json:"kgs"`
					Rev json.RawMessage            `json:"frames_rev"`
				}
				if resp.StatusCode == 200 && json.NewDecoder(io.LimitReader(resp.Body, 8<<20)).Decode(&d) == nil {
					rev, moved, ok = string(d.Rev), len(d.KGs), true
				}
				resp.Body.Close()
			}
		}
		s.mu.Lock()
		s.at, s.busy = time.Now(), false
		s.checks++
		if ok {
			if s.seen && rev != s.rev {
				treeFrameGen.Add(1) // every cached tree set is from before: read again
				s.bumps++
			}
			s.seen, s.rev, s.moved = true, rev, moved
		}
		s.mu.Unlock()
	}()
}

// treeFrameAnswer — a relayed answer of the peer: moved_m in it means a copy
// (the reporter's, maybe ours) is from before a correction.
func treeFrameAnswer(out []byte) {
	if bytes.Contains(out, []byte(`"moved_m"`)) {
		treeFrameCheck(true)
	}
}

// TreeFrameStats — one line for debug/inflight.
func TreeFrameStats() map[string]interface{} {
	s := &treeFrameState
	s.mu.Lock()
	defer s.mu.Unlock()
	return map[string]interface{}{"frames_rev": s.rev, "moved_kgs": s.moved, "gen": treeFrameGen.Load(), "checks": s.checks, "bumps": s.bumps, "moved_answers": s.hints}
}

// treeFrameKG — what the peer says about the points of a file in one KG.
type treeFrameKG struct {
	Frame     string    `json:"frame"`   // the frame the file is in
	Current   string    `json:"current"` // the frame the KG is in today
	Confident *bool     `json:"confident"`
	Stale     string    `json:"stale"`   // the frame of a copy from before the correction
	Moved     []float64 `json:"moved_m"` // EPSG:3035 metres from a Stale position to today's
}

// treeFrameAsk — POST /trees/frame: per KG, which frame a file's points are
// in. Only KGs that moved are listed; an empty map = nothing to say.
func treeFrameAsk(ctx context.Context, pts [][2]float64) (map[string]treeFrameKG, error) {
	u := treeFrameURL()
	if u == "" {
		return nil, errors.New("no tree source configured")
	}
	out := map[string]treeFrameKG{}
	for len(pts) > 0 {
		n := min(len(pts), treeFramePoints)
		body, _ := json.Marshal(map[string]interface{}{"points": pts[:n]})
		pts = pts[n:]
		c, cancel := context.WithTimeout(ctx, 30*time.Second)
		req, _ := http.NewRequestWithContext(c, http.MethodPost, u, bytes.NewReader(body))
		req.Header.Set("Content-Type", "application/json")
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			cancel()
			return nil, err
		}
		var d struct {
			KGs map[string]treeFrameKG `json:"kgs"`
		}
		err = json.NewDecoder(io.LimitReader(resp.Body, 8<<20)).Decode(&d)
		code := resp.StatusCode
		resp.Body.Close()
		cancel()
		if code != 200 || err != nil {
			return nil, errors.New("frame question refused")
		}
		for k, v := range d.KGs {
			if old, ok := out[k]; ok && old.Frame != v.Frame {
				f := false
				v.Confident = &f // two parts of one file disagree: say nothing for this KG
			}
			out[k] = v
		}
	}
	return out, nil
}

// treeFrameOf — the frame to send for a file's point, "" = none (let the peer
// decide). We hold no KG outlines, only their boxes: the candidates of the
// point must agree — a KG the peer does not list never moved ("p0") — and a KG
// the peer is unsure about says nothing.
// treeFrameKGsAt — the KG candidates of a point; a var so tests decide.
var treeFrameKGsAt = KGCodesForPointMem

func treeFrameOf(kgs map[string]treeFrameKG, lon, lat float64) string {
	if len(kgs) == 0 {
		return ""
	}
	got, listed := "", false
	for _, kg := range treeFrameKGsAt(lon, lat) {
		f := treeFrameZero
		if v, ok := kgs[kg]; ok {
			if (v.Confident != nil && !*v.Confident) || treeFrameToken(v.Frame) == "" {
				return ""
			}
			f, listed = v.Frame, true
		}
		if got != "" && got != f {
			return ""
		}
		got = f
	}
	if !listed {
		return ""
	}
	return got
}

// EPSG:3035 (ETRS89-LAEA Europe, the CRS the peer shifts in) ⇄ lon, lat.
// Snyder's ellipsoidal Lambert azimuthal equal-area on GRS80, origin 52° N
// 10° E at (4321000, 3210000); agrees with PROJ to a millimetre in Austria.
const (
	laeaA  = 6378137.0
	laeaE2 = 0.00669438002290
)

var laeaE, laeaQp, laeaB0, laeaRq, laeaD = func() (e, qp, b0, rq, d float64) {
	e = math.Sqrt(laeaE2)
	f0 := 52 * math.Pi / 180
	s0 := math.Sin(f0)
	qp = (1 - laeaE2) * (1/(1-laeaE2) - math.Log((1-e)/(1+e))/(2*e))
	q0 := (1 - laeaE2) * (s0/(1-laeaE2*s0*s0) - math.Log((1-e*s0)/(1+e*s0))/(2*e))
	b0 = math.Asin(q0 / qp)
	rq = laeaA * math.Sqrt(qp/2)
	d = laeaA * math.Cos(f0) / (math.Sqrt(1-laeaE2*s0*s0) * rq * math.Cos(b0))
	return
}()

func laeaFwd(lon, lat float64) (x, y float64) {
	s := math.Sin(lat * math.Pi / 180)
	q := (1 - laeaE2) * (s/(1-laeaE2*s*s) - math.Log((1-laeaE*s)/(1+laeaE*s))/(2*laeaE))
	b := math.Asin(q / laeaQp)
	dl := (lon - 10) * math.Pi / 180
	k := laeaRq * math.Sqrt(2/(1+math.Sin(laeaB0)*math.Sin(b)+math.Cos(laeaB0)*math.Cos(b)*math.Cos(dl)))
	return 4321000 + k*laeaD*math.Cos(b)*math.Sin(dl),
		3210000 + k/laeaD*(math.Cos(laeaB0)*math.Sin(b)-math.Sin(laeaB0)*math.Cos(b)*math.Cos(dl))
}

func laeaInv(X, Y float64) (lon, lat float64) {
	x, y := X-4321000, Y-3210000
	rho := math.Hypot(x/laeaD, laeaD*y)
	if rho == 0 {
		return 10, 52
	}
	c := 2 * math.Asin(rho/(2*laeaRq))
	b := math.Asin(math.Cos(c)*math.Sin(laeaB0) + laeaD*y*math.Sin(c)*math.Cos(laeaB0)/rho)
	e4, e6 := laeaE2*laeaE2, laeaE2*laeaE2*laeaE2
	lon = 10 + math.Atan2(x*math.Sin(c), laeaD*rho*math.Cos(laeaB0)*math.Cos(c)-laeaD*laeaD*y*math.Sin(laeaB0)*math.Sin(c))*180/math.Pi
	lat = (b + (laeaE2/3+31*e4/180+517*e6/5040)*math.Sin(2*b) + (23*e4/360+251*e6/3780)*math.Sin(4*b) + 761*e6/45360*math.Sin(6*b)) * 180 / math.Pi
	return
}

// treeFrameShifts — for a file's point: the corrections it may be from
// before (KGs round the point the peer lists with a shift), as today's
// position of that point + the token of the copy before.
type treeFrameShift struct {
	lon, lat float64 // where the same tree is today
	stale    string
}

func treeFrameShifts(kgs map[string]treeFrameKG, lon, lat float64) []treeFrameShift {
	var out []treeFrameShift
	for _, kg := range treeFrameKGsAt(lon, lat) {
		v, ok := kgs[kg]
		if !ok || len(v.Moved) != 2 || treeFrameToken(v.Stale) == "" || (v.Moved[0] == 0 && v.Moved[1] == 0) || math.Hypot(v.Moved[0], v.Moved[1]) > 100 {
			continue
		}
		dup := false
		for _, o := range out {
			dup = dup || o.stale == v.Stale
		}
		if dup {
			continue
		}
		x, y := laeaFwd(lon, lat)
		lo, la := laeaInv(x+v.Moved[0], y+v.Moved[1])
		out = append(out, treeFrameShift{lo, la, v.Stale})
	}
	return out
}
