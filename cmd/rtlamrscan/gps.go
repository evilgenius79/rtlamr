package main

import (
	"bufio"
	"bytes"
	"context"
	"fmt"
	"log"
	"strconv"
	"strings"
	"sync"
	"time"

	"go.bug.st/serial"
)

// Baud rates tried during auto-detection, most likely first. 460800 leads
// because that's what modern u-blox based pucks ship at.
var gpsBaudCandidates = []int{460800, 9600, 115200, 38400, 57600, 4800}

// gpsFix is the most recent position report from the GPS receiver.
type gpsFix struct {
	Lat, Lon float64
	Quality  int
	NumSats  int
	HDOP     float64
	AltM     float64
	when     time.Time
}

// gpsReader consumes NMEA sentences from a serial port in the background and
// keeps the latest valid fix.
type gpsReader struct {
	mu     sync.Mutex
	fix    gpsFix
	valid  bool
	talker string // e.g. "GN", "GP"; first talker seen on a GGA sentence
	hadFix bool   // whether a fix was ever acquired (for log messages)

	// Ground speed and course from RMC sentences.
	speedKmh, course float64
	motionWhen       time.Time
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

// gpsStatus is the dashboard-facing view of the GPS state.
type gpsStatus struct {
	HasFix     bool    `json:"hasFix"`
	EverHadFix bool    `json:"everHadFix"`
	Lat        float64 `json:"lat"`
	Lon        float64 `json:"lon"`
	Quality    int     `json:"quality"`
	NumSats    int     `json:"numSats"`
	HDOP       float64 `json:"hdop"`
	AltM       float64 `json:"altM"`
	SpeedKmh   float64 `json:"speedKmh"`
	Course     float64 `json:"course"`
	AgeSec     float64 `json:"ageSec"`
	Talker     string  `json:"talker"`
}

func (g *gpsReader) statusSnapshot(maxAge time.Duration) gpsStatus {
	g.mu.Lock()
	defer g.mu.Unlock()

	st := gpsStatus{EverHadFix: g.hadFix, Talker: g.talker}
	if g.valid {
		age := time.Since(g.fix.when)
		if age <= maxAge {
			st.HasFix = true
			st.Lat, st.Lon = g.fix.Lat, g.fix.Lon
			st.Quality, st.NumSats, st.HDOP = g.fix.Quality, g.fix.NumSats, g.fix.HDOP
			st.AltM = g.fix.AltM
			st.AgeSec = age.Seconds()
			if time.Since(g.motionWhen) <= maxAge {
				st.SpeedKmh, st.Course = g.speedKmh, g.course
			}
		}
	}
	return st
}

// status describes the reader's state for preflight/diagnostic output.
func (g *gpsReader) status() string {
	g.mu.Lock()
	defer g.mu.Unlock()

	talker := "no NMEA seen yet"
	if g.talker != "" {
		talker = "$" + g.talker + "GGA"
	}
	if !g.valid {
		return fmt.Sprintf("no fix (%s)", talker)
	}
	return fmt.Sprintf("fix: %.6f,%.6f quality=%d sats=%d hdop=%.1f (%s)",
		g.fix.Lat, g.fix.Lon, g.fix.Quality, g.fix.NumSats, g.fix.HDOP, talker)
}

// detectGPSPort opens the port and finds a baud rate producing valid NMEA.
// A forced baud (> 0) is the only one tried.
func detectGPSPort(portName string, forced int) (serial.Port, int, error) {
	candidates := gpsBaudCandidates
	if forced > 0 {
		candidates = []int{forced}
	}

	port, err := serial.Open(portName, &serial.Mode{BaudRate: candidates[0]})
	if err != nil {
		return nil, 0, err
	}

	buf := make([]byte, 512)
	for _, baud := range candidates {
		if err := port.SetMode(&serial.Mode{BaudRate: baud}); err != nil {
			continue
		}
		port.SetReadTimeout(300 * time.Millisecond)
		port.ResetInputBuffer()

		var window bytes.Buffer
		deadline := time.Now().Add(2 * time.Second)
		for time.Now().Before(deadline) {
			n, err := port.Read(buf)
			if err != nil {
				break
			}
			window.Write(buf[:n])
			if containsValidNMEA(window.String()) {
				// Back to blocking reads for the long-running reader.
				port.SetReadTimeout(serial.NoTimeout)
				return port, baud, nil
			}
			// Keep the scan window bounded on noisy input.
			if window.Len() > 8192 {
				tail := window.String()[window.Len()-1024:]
				window.Reset()
				window.WriteString(tail)
			}
		}
	}

	port.Close()
	if forced > 0 {
		return nil, 0, fmt.Errorf("no valid NMEA at %d baud - try -gpsbaud 0 to auto-detect", forced)
	}
	return nil, 0, fmt.Errorf("no valid NMEA at any of %v baud - is this the right port?", gpsBaudCandidates)
}

// containsValidNMEA reports whether the buffer holds at least one complete
// NMEA sentence with a correct checksum.
func containsValidNMEA(s string) bool {
	for _, line := range strings.Split(s, "\n") {
		if _, ok := nmeaChecksumOK(strings.TrimSpace(line)); ok {
			return true
		}
	}
	return false
}

// startGPS opens the named serial port (auto-detecting the baud rate unless
// one is forced) and consumes NMEA from it until the context is cancelled.
func startGPS(ctx context.Context, portName string, forcedBaud int) (*gpsReader, error) {
	port, baud, err := detectGPSPort(portName, forcedBaud)
	if err != nil {
		return nil, err
	}
	log.Printf("[gps] valid NMEA on %s at %d baud", portName, baud)

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
		if speedKmh, course, rmcValid, rmcOK := parseRMC(s); rmcOK && rmcValid {
			g.mu.Lock()
			g.speedKmh, g.course = speedKmh, course
			g.motionWhen = time.Now()
			g.mu.Unlock()
		}
		return
	}
	fix.when = time.Now()

	g.mu.Lock()
	if g.talker == "" && len(s) >= 3 {
		g.talker = s[1:3]
		log.Printf("[gps] talker $%sGGA", g.talker)
	}
	if valid && !g.hadFix {
		g.hadFix = true
		log.Printf("[gps] fix acquired: %.6f,%.6f (%d sats)", fix.Lat, fix.Lon, fix.NumSats)
	}
	if !valid && g.valid {
		log.Printf("[gps] fix lost")
	}
	g.fix, g.valid = fix, valid
	g.mu.Unlock()
}

// runGPSTest is the -gpstest preflight: it prints live fix status once per
// second so the puck, baud, talker, and sky view can be verified before a
// drive. Runs until Ctrl+C or two minutes, whichever comes first.
func runGPSTest(ctx context.Context, g *gpsReader) {
	log.Printf("[gps] test mode: watching for fixes for up to 2 minutes, Ctrl+C to stop")
	log.Printf("[gps] no fix indoors is normal - the antenna needs sky view")

	deadline := time.After(2 * time.Minute)
	tick := time.NewTicker(time.Second)
	defer tick.Stop()

	for {
		select {
		case <-ctx.Done():
			return
		case <-deadline:
			log.Printf("[gps] test finished: %s", g.status())
			return
		case <-tick.C:
			log.Printf("[gps] %s", g.status())
		}
	}
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

// parseGGA parses a GGA sentence from any talker ($GPGGA, $GNGGA, $GLGGA,
// $GAGGA, ...): the sentence type is matched on the "GGA" part alone. ok
// reports whether the sentence was a well-formed GGA at all; valid reports
// whether it carries a usable fix.
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
	if len(fields) > 9 {
		fix.AltM, _ = strconv.ParseFloat(fields[9], 64)
	}
	return fix, true, true
}

// parseRMC parses an RMC sentence from any talker, returning ground speed
// (km/h) and course over ground (degrees true). ok reports whether the
// sentence was a well-formed RMC; valid reports whether its data is usable
// (status "A").
func parseRMC(s string) (speedKmh, course float64, valid, ok bool) {
	payload, ck := nmeaChecksumOK(s)
	if !ck {
		return 0, 0, false, false
	}

	fields := strings.Split(payload, ",")
	if len(fields) < 9 || len(fields[0]) != 5 || fields[0][2:] != "RMC" {
		return 0, 0, false, false
	}

	// Field layout: 0 talker, 1 utc, 2 status, 3 lat, 4 N/S, 5 lon, 6 E/W,
	// 7 speed over ground in knots, 8 course over ground in degrees.
	if fields[2] != "A" {
		return 0, 0, false, true
	}

	knots, _ := strconv.ParseFloat(fields[7], 64)
	course, _ = strconv.ParseFloat(fields[8], 64)
	return knots * 1.852, course, true, true
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
