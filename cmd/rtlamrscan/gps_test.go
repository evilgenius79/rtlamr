package main

import (
	"math"
	"testing"
)

func TestParseGGA(t *testing.T) {
	// Real-world style GGA: 48°07.038'N, 11°31.000'E, quality 1, 8 sats.
	fix, valid, ok := parseGGA("$GPGGA,123519,4807.038,N,01131.000,E,1,08,0.9,545.4,M,46.9,M,,*47")
	if !ok || !valid {
		t.Fatalf("expected valid GGA, got ok=%v valid=%v", ok, valid)
	}
	if math.Abs(fix.Lat-48.1173) > 0.0001 || math.Abs(fix.Lon-11.516666) > 0.0001 {
		t.Fatalf("bad coordinates: %.6f, %.6f", fix.Lat, fix.Lon)
	}
	if fix.Quality != 1 || fix.NumSats != 8 || math.Abs(fix.HDOP-0.9) > 0.001 {
		t.Fatalf("bad metadata: %+v", fix)
	}
}

func TestParseGGASouthWest(t *testing.T) {
	fix, valid, ok := parseGGA("$GNGGA,123519,3345.500,S,07040.250,W,2,12,1.1,10.0,M,0.0,M,,*5F")
	if !ok || !valid {
		t.Fatalf("expected valid GGA, got ok=%v valid=%v", ok, valid)
	}
	if fix.Lat >= 0 || fix.Lon >= 0 {
		t.Fatalf("expected negative coordinates, got %.6f, %.6f", fix.Lat, fix.Lon)
	}
	if math.Abs(fix.Lat+33.758333) > 0.0001 || math.Abs(fix.Lon+70.670833) > 0.0001 {
		t.Fatalf("bad coordinates: %.6f, %.6f", fix.Lat, fix.Lon)
	}
}

func TestParseGGANoFix(t *testing.T) {
	// Quality 0 with empty coordinates: well-formed but not a usable fix.
	_, valid, ok := parseGGA("$GPGGA,123519,,,,,0,00,,,M,,M,,*6B")
	if !ok {
		t.Fatal("expected sentence to be recognized as GGA")
	}
	if valid {
		t.Fatal("quality-0 sentence must not produce a valid fix")
	}
}

func TestParseGGARejects(t *testing.T) {
	cases := []string{
		"",
		"$GPRMC,123519,A,4807.038,N,01131.000,E,022.4,084.4,230394,003.1,W*6A", // not GGA
		"$GPGGA,123519,4807.038,N,01131.000,E,1,08,0.9,545.4,M,46.9,M,,*00",    // bad checksum
		"garbage",
	}
	for _, c := range cases {
		if _, valid, ok := parseGGA(c); ok && valid {
			t.Fatalf("expected rejection of %q", c)
		}
	}
}
