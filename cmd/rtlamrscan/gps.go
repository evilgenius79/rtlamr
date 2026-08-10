package main

import (
	"bufio"
	"context"
	"log"
	"strconv"
	"strings"
	"sync"
	"time"

	"go.bug.st/serial"
)

// gpsFix is the most recent position report from the GPS receiver.
type gpsFix struct {
	Lat, Lon float64
	Quality  int
	NumSats  int
	HDOP     float64
	when     time.Time
}

// gpsReader consumes NMEA sentences from a serial port in the background and
// keeps the latest valid fix.
type gpsReader struct {
	mu    sync.Mutex
	fix   gpsFix
	valid bool
}

// current returns the latest fix, or ok=false when there is no valid fix or
// the last one is older than maxAge (e.g. the puck was unplugged mid-drive).
func (g *gpsReader) current(maxAge time.Duration) (gpsFix, bool) {
	g.mu.Lock()
	defer g.mu.Unlock()
	if !g.valid || time.Since(g.fix.when) > maxAge {
		return gpsFix{}, false
	}
	return g.fix, true
}

func (g *gpsReader) update(fix gpsFix, valid bool) {
	g.mu.Lock()
	defer g.mu.Unlock()
	g.fix, g.valid = fix, valid
}

// startGPS opens the named serial port and consumes NMEA from it until the
// context is cancelled.
func startGPS(ctx context.Context, portName string, baud int) (*gpsReader, error) {
	port, err := serial.Open(portName, &serial.Mode{BaudRate: baud})
	if err != nil {
		return nil, err
	}

	g := &gpsReader{}

	// Closing the port unblocks the scanner when we shut down.
	go func() {
		<-ctx.Done()
		port.Close()
	}()

	go func() {
		scanner := bufio.NewScanner(port)
		for scanner.Scan() {
			if ctx.Err() != nil {
				return
			}
			g.handleSentence(strings.TrimSpace(scanner.Text()))
		}
		if ctx.Err() == nil {
			log.Printf("[gps] serial port closed: %v", scanner.Err())
		}
	}()

	return g, nil
}

func (g *gpsReader) handleSentence(s string) {
	fix, valid, ok := parseGGA(s)
	if !ok {
		return
	}
	fix.when = time.Now()
	g.update(fix, valid)
}

// nmeaChecksumOK verifies the $...*hh sentence checksum and returns the
// payload between '$' and '*'.
func nmeaChecksumOK(s string) (string, bool) {
	if len(s) < 4 || s[0] != '$' {
		return "", false
	}
	star := strings.LastIndexByte(s, '*')
	if star < 0 || star+3 > len(s) {
		return "", false
	}

	var sum byte
	for i := 1; i < star; i++ {
		sum ^= s[i]
	}
	want, err := strconv.ParseUint(s[star+1:star+3], 16, 8)
	if err != nil || byte(want) != sum {
		return "", false
	}
	return s[1:star], true
}

// parseGGA parses a GGA sentence (any talker: GPGGA, GNGGA, ...). ok reports
// whether the sentence was a well-formed GGA at all; valid reports whether it
// carries a usable fix.
func parseGGA(s string) (fix gpsFix, valid, ok bool) {
	payload, ck := nmeaChecksumOK(s)
	if !ck {
		return fix, false, false
	}

	fields := strings.Split(payload, ",")
	if len(fields) < 9 || len(fields[0]) != 5 || fields[0][2:] != "GGA" {
		return fix, false, false
	}

	// Field layout: 0 talker, 1 utc, 2 lat, 3 N/S, 4 lon, 5 E/W,
	// 6 fix quality, 7 satellites, 8 hdop, ...
	quality, err := strconv.Atoi(fields[6])
	if err != nil || quality == 0 {
		return fix, false, true // well-formed, but no fix
	}

	lat, latErr := parseNMEACoord(fields[2], fields[3], 2)
	lon, lonErr := parseNMEACoord(fields[4], fields[5], 3)
	if latErr != nil || lonErr != nil {
		return fix, false, true
	}

	fix.Lat, fix.Lon = lat, lon
	fix.Quality = quality
	fix.NumSats, _ = strconv.Atoi(fields[7])
	fix.HDOP, _ = strconv.ParseFloat(fields[8], 64)
	return fix, true, true
}

// parseNMEACoord converts NMEA ddmm.mmmm / dddmm.mmmm plus hemisphere into
// decimal degrees. degDigits is 2 for latitude, 3 for longitude.
func parseNMEACoord(value, hemisphere string, degDigits int) (float64, error) {
	if len(value) < degDigits {
		return 0, strconv.ErrSyntax
	}

	deg, err := strconv.ParseFloat(value[:degDigits], 64)
	if err != nil {
		return 0, err
	}
	min, err := strconv.ParseFloat(value[degDigits:], 64)
	if err != nil {
		return 0, err
	}

	coord := deg + min/60
	switch hemisphere {
	case "S", "W":
		coord = -coord
	case "N", "E":
	default:
		return 0, strconv.ErrSyntax
	}
	return coord, nil
}
