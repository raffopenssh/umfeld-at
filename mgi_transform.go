// mgi_transform.go — EPSG:31255 (MGI / Austria GK Central) → EPSG:4326 (WGS 84).
//
// BEV publishes the DLM (Digitales Landschaftsmodell) in the national
// Gauß-Krüger system on the Bessel 1841 ellipsoid, MGI datum. The API serves
// WGS 84, so every DLM coordinate has to go through
//
//	GK inverse (Transverse Mercator, Bessel)  →  geodetic MGI
//	→ ECEF  →  7-parameter Helmert (EPSG:1618, position-vector)  →  ECEF WGS84
//	→ geodetic WGS84
//
// The Helmert parameters are the BEV/EPSG ones for Austria
// (dX 577.326 m, dY 90.129 m, dZ 463.919 m, rX 5.137″, rY 1.474″, rZ 5.297″,
// ds 2.4232 ppm). Verified against `gdaltransform -s_srs EPSG:31255 -t_srs
// EPSG:4326` to well under a metre (mgi_transform_test.go) — the source data's
// own positional accuracy is ±50 m, so this is not the limiting term.
//
// Doing it natively (instead of shelling out to ogr2ogr) keeps the toponym
// import a pure-Go, one-binary operation and lets a test pin the numbers.
package main

import "math"

// Bessel 1841
const (
	besselA = 6377397.155
	besselF = 1.0 / 299.1528128
)

// WGS 84
const (
	wgsA = 6378137.0
	wgsF = 1.0 / 298.257223563
)

// MGI / Austria GK Central (EPSG:31255) projection parameters.
const (
	gkCentralLon0    = 13.0 + 20.0/60.0 // 13°20′ E
	gkCentralK0      = 1.0
	gkCentralFalseE  = 0.0
	gkCentralFalseN  = -5000000.0
	mgiToWGS84dX     = 577.326
	mgiToWGS84dY     = 90.129
	mgiToWGS84dZ     = 463.919
	mgiToWGS84rXsec  = 5.137
	mgiToWGS84rYsec  = 1.474
	mgiToWGS84rZsec  = 5.297
	mgiToWGS84dsPPM  = 2.4232
	arcsecToRad      = math.Pi / (180.0 * 3600.0)
	gkCentralMinEast = -400000.0 // sanity bounds for Austria
	gkCentralMaxEast = 400000.0
	gkCentralMinNrth = 100000.0
	gkCentralMaxNrth = 500000.0
)

// tmInverse converts Transverse Mercator easting/northing (metres, false
// origin already removed by the caller) to geodetic lat/lon (radians) on the
// ellipsoid (a, f) with central meridian lon0 (radians) and scale k0.
// Snyder, Map Projections — A Working Manual, eq. 8-11 … 8-25 (lat0 = 0).
func tmInverse(x, y, a, f, lon0, k0 float64) (lat, lon float64) {
	e2 := f * (2 - f)
	ep2 := e2 / (1 - e2)

	// Footpoint latitude via the rectifying latitude series.
	M := y / k0
	mu := M / (a * (1 - e2/4 - 3*e2*e2/64 - 5*e2*e2*e2/256))
	e1 := (1 - math.Sqrt(1-e2)) / (1 + math.Sqrt(1-e2))
	phi1 := mu +
		(3*e1/2-27*e1*e1*e1/32)*math.Sin(2*mu) +
		(21*e1*e1/16-55*e1*e1*e1*e1/32)*math.Sin(4*mu) +
		(151*e1*e1*e1/96)*math.Sin(6*mu) +
		(1097*e1*e1*e1*e1/512)*math.Sin(8*mu)

	sin1, cos1 := math.Sin(phi1), math.Cos(phi1)
	tan1 := sin1 / cos1
	C1 := ep2 * cos1 * cos1
	T1 := tan1 * tan1
	N1 := a / math.Sqrt(1-e2*sin1*sin1)
	R1 := a * (1 - e2) / math.Pow(1-e2*sin1*sin1, 1.5)
	D := x / (N1 * k0)

	lat = phi1 - (N1*tan1/R1)*(D*D/2-
		(5+3*T1+10*C1-4*C1*C1-9*ep2)*D*D*D*D/24+
		(61+90*T1+298*C1+45*T1*T1-252*ep2-3*C1*C1)*D*D*D*D*D*D/720)
	lon = lon0 + (D-(1+2*T1+C1)*D*D*D/6+
		(5-2*C1+28*T1-3*C1*C1+8*ep2+24*T1*T1)*D*D*D*D*D/120)/cos1
	return lat, lon
}

// geodeticToECEF converts lat/lon (radians), h (m) on ellipsoid (a, f).
func geodeticToECEF(lat, lon, h, a, f float64) (x, y, z float64) {
	e2 := f * (2 - f)
	sinLat, cosLat := math.Sin(lat), math.Cos(lat)
	N := a / math.Sqrt(1-e2*sinLat*sinLat)
	x = (N + h) * cosLat * math.Cos(lon)
	y = (N + h) * cosLat * math.Sin(lon)
	z = (N*(1-e2) + h) * sinLat
	return
}

// ecefToGeodetic converts ECEF to lat/lon (radians) on ellipsoid (a, f);
// iterative, converges in a handful of steps for terrestrial points.
func ecefToGeodetic(x, y, z, a, f float64) (lat, lon float64) {
	e2 := f * (2 - f)
	lon = math.Atan2(y, x)
	p := math.Hypot(x, y)
	lat = math.Atan2(z, p*(1-e2))
	for i := 0; i < 10; i++ {
		sinLat := math.Sin(lat)
		N := a / math.Sqrt(1-e2*sinLat*sinLat)
		h := p/math.Cos(lat) - N
		newLat := math.Atan2(z, p*(1-e2*N/(N+h)))
		if math.Abs(newLat-lat) < 1e-13 {
			lat = newLat
			break
		}
		lat = newLat
	}
	return
}

// helmertPositionVector applies the EPSG:9606 position-vector 7-parameter
// transformation (small-angle form, as EPSG specifies for these parameters).
func helmertPositionVector(x, y, z float64) (float64, float64, float64) {
	rx := mgiToWGS84rXsec * arcsecToRad
	ry := mgiToWGS84rYsec * arcsecToRad
	rz := mgiToWGS84rZsec * arcsecToRad
	s := 1 + mgiToWGS84dsPPM*1e-6
	x2 := mgiToWGS84dX + s*(x-rz*y+ry*z)
	y2 := mgiToWGS84dY + s*(rz*x+y-rx*z)
	z2 := mgiToWGS84dZ + s*(-ry*x+rx*y+z)
	return x2, y2, z2
}

// GKCentralToWGS84 converts an EPSG:31255 easting/northing pair to WGS 84
// lon/lat in degrees. ok=false when the input is outside any plausible
// Austrian extent (a defensive check against swapped axes or wrong CRS).
func GKCentralToWGS84(easting, northing float64) (lon, lat float64, ok bool) {
	if math.IsNaN(easting) || math.IsNaN(northing) ||
		easting < gkCentralMinEast || easting > gkCentralMaxEast ||
		northing < gkCentralMinNrth || northing > gkCentralMaxNrth {
		return 0, 0, false
	}
	x := easting - gkCentralFalseE
	y := northing - gkCentralFalseN
	latM, lonM := tmInverse(x, y, besselA, besselF, gkCentralLon0*math.Pi/180, gkCentralK0)
	ex, ey, ez := geodeticToECEF(latM, lonM, 0, besselA, besselF)
	wx, wy, wz := helmertPositionVector(ex, ey, ez)
	latW, lonW := ecefToGeodetic(wx, wy, wz, wgsA, wgsF)
	return lonW * 180 / math.Pi, latW * 180 / math.Pi, true
}

// GKToWGS84 converts an MGI Gauß-Krüger easting/northing in any of the three
// Austrian zones — EPSG:31254 (M28, 10°20′), 31255 (M31, 13°20′), 31256
// (M34, 16°20′) — to WGS 84 lon/lat. Same chain as GKCentralToWGS84; the
// central meridian is the only difference. Used by the BEV Adressregister
// import, whose rows carry the zone per address.
func GKToWGS84(epsg int, easting, northing float64) (lon, lat float64, ok bool) {
	var lon0 float64
	switch epsg {
	case 31254:
		lon0 = 10.0 + 20.0/60.0
	case 31255:
		lon0 = gkCentralLon0
	case 31256:
		lon0 = 16.0 + 20.0/60.0
	default:
		return 0, 0, false
	}
	if math.IsNaN(easting) || math.IsNaN(northing) ||
		easting < gkCentralMinEast || easting > gkCentralMaxEast ||
		northing < gkCentralMinNrth || northing > gkCentralMaxNrth {
		return 0, 0, false
	}
	latM, lonM := tmInverse(easting, northing-gkCentralFalseN, besselA, besselF, lon0*math.Pi/180, gkCentralK0)
	ex, ey, ez := geodeticToECEF(latM, lonM, 0, besselA, besselF)
	wx, wy, wz := helmertPositionVector(ex, ey, ez)
	latW, lonW := ecefToGeodetic(wx, wy, wz, wgsA, wgsF)
	lon, lat = lonW*180/math.Pi, latW*180/math.Pi
	if lon < 9 || lon > 17.5 || lat < 46 || lat > 49.2 {
		return 0, 0, false
	}
	return lon, lat, true
}
