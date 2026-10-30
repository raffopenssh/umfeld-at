package main

import (
	"math"
	"testing"
)

// Reference values from `gdaltransform -s_srs EPSG:31255 -t_srs EPSG:4326`
// (PROJ, EPSG:1618 Helmert). Tolerance 0.5 m — the DLM's own accuracy is ±50 m.
func TestGKCentralToWGS84MatchesPROJ(t *testing.T) {
	cases := []struct{ e, n, lon, lat float64 }{
		{0, 0, 13.3327467758259, 45.1397918384675},
		{-200000, 300000, 10.6623587716017, 47.8074210645743},
		{250000, 150000, 16.5861362125118, 46.4431431806667},
	}
	for _, c := range cases {
		// (0,0) is outside the sanity bounds; call the inner pipeline directly.
		x, y := c.e-gkCentralFalseE, c.n-gkCentralFalseN
		latM, lonM := tmInverse(x, y, besselA, besselF, gkCentralLon0*math.Pi/180, gkCentralK0)
		ex, ey, ez := geodeticToECEF(latM, lonM, 0, besselA, besselF)
		wx, wy, wz := helmertPositionVector(ex, ey, ez)
		latW, lonW := ecefToGeodetic(wx, wy, wz, wgsA, wgsF)
		lon, lat := lonW*180/math.Pi, latW*180/math.Pi
		dm := haversine(lat, lon, c.lat, c.lon)
		if dm > 0.5 {
			t.Errorf("(%g,%g): got %.8f,%.8f want %.8f,%.8f — off by %.2f m", c.e, c.n, lon, lat, c.lon, c.lat, dm)
		}
	}
	if _, _, ok := GKCentralToWGS84(5e6, 5e6); ok {
		t.Errorf("out-of-range input must be rejected")
	}
}
