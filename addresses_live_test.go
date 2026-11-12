package main

import (
	"fmt"
	"os"
	"strings"
	"testing"
	"time"
)

// TestAddressesLive — prints the answers for ADDR_Q (one query per line,
// "text|w,s,e,n") against the local store; skipped without ADDR_Q.
func TestAddressesLive(t *testing.T) {
	qs := os.Getenv("ADDR_Q")
	db := addressesDB()
	if qs == "" || db == nil {
		t.Skip("no ADDR_Q / store")
	}
	if b, err := os.ReadFile(qs); err == nil {
		qs = string(b)
	}
	ix := addressesIndex(db)
	fmt.Println(AddressesIndexStats())
	_ = ix
	for _, line := range strings.Split(strings.TrimSpace(qs), "\n") {
		p := strings.SplitN(line, "|", 2)
		var bbox *[4]float64
		var c *[2]float64
		if len(p) == 2 && p[1] != "" {
			var b [4]float64
			fmt.Sscanf(p[1], "%f,%f,%f,%f", &b[0], &b[1], &b[2], &b[3])
			bbox = &b
			c = &[2]float64{(b[0] + b[2]) / 2, (b[1] + b[3]) / 2}
		}
		t0 := time.Now()
		hits, approx, err := addressesSearch(db, p[0], bbox, c, 5)
		fmt.Printf("== %s  [%s approx=%v err=%v]\n", line, time.Since(t0).Round(10*time.Microsecond), approx, err)
		for _, h := range hits {
			fmt.Printf("   %-7s %s | %s  full=%v view=%v\n", h.Kind, h.Label, h.Sub, h.Full, h.InView)
		}
	}
}
