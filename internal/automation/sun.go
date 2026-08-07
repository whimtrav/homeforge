package automation

import (
	"math"
	"time"
)

// SunTimes returns today's sunrise and sunset (in now's timezone) for the given
// latitude/longitude in degrees (lon east-positive, e.g. -108.5 for Billings MT).
// Standard NOAA sunrise equation — accurate to ~1 minute, no external calls.
// Verified 2026-08-04 for 45.7833/-108.5007: sunrise 06:00, sunset 20:39 MDT.
func SunTimes(lat, lon float64, now time.Time) (sunrise, sunset time.Time) {
	loc := now.Location()
	y, m, d := now.Date()
	rad := math.Pi / 180
	jd := julianDate(y, int(m), d)
	n := math.Round(jd - 2451545.0 + 0.0008)
	jStar := n - lon/360.0 // mean solar noon (lon east-positive)
	meanAnom := math.Mod(357.5291+0.98560028*jStar, 360)
	center := 1.9148*math.Sin(meanAnom*rad) + 0.0200*math.Sin(2*meanAnom*rad) + 0.0003*math.Sin(3*meanAnom*rad)
	lambda := math.Mod(meanAnom+center+180+102.9372, 360)
	jTransit := 2451545.0 + jStar + 0.0053*math.Sin(meanAnom*rad) - 0.0069*math.Sin(2*lambda*rad)
	sinDelta := math.Sin(lambda*rad) * math.Sin(23.44*rad)
	cosDelta := math.Cos(math.Asin(sinDelta))
	cosOmega := (math.Sin(-0.833*rad) - math.Sin(lat*rad)*sinDelta) / (math.Cos(lat*rad) * cosDelta)
	omega := math.Acos(clampF(cosOmega, -1, 1)) / rad
	return julianToTime(jTransit-omega/360.0, loc), julianToTime(jTransit+omega/360.0, loc)
}

func clampF(x, lo, hi float64) float64 {
	if x < lo {
		return lo
	}
	if x > hi {
		return hi
	}
	return x
}

// julianDate returns the Julian Date at 00:00 UTC for the given calendar date.
func julianDate(year, month, day int) float64 {
	if month <= 2 {
		year--
		month += 12
	}
	a := year / 100
	b := 2 - a + a/4
	return math.Floor(365.25*float64(year+4716)) + math.Floor(30.6001*float64(month+1)) + float64(day) + float64(b) - 1524.5
}

func julianToTime(jd float64, loc *time.Location) time.Time {
	return time.Unix(int64(math.Round((jd-2440587.5)*86400.0)), 0).In(loc)
}
