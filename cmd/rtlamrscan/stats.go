package main

import (
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
)

// recentRow is one decoded burst, kept for the dashboard's live feed.
type recentRow struct {
	Time    string `json:"time"`
	Radio   int    `json:"radio"`
	ID      string `json:"id"`
	RSSI    string `json:"rssi"`
	Leak    int    `json:"leak"`
	LeakNow int    `json:"leakNow"`
}

// meterStat aggregates everything seen from one meter.
type meterStat struct {
	ID          string `json:"id"`
	Bursts      int    `json:"bursts"`
	Leak        int    `json:"leak"`    // worst seen
	LeakNow     int    `json:"leakNow"` // worst seen
	LastRSSI    string `json:"lastRssi"`
	LastSeenSec int64  `json:"lastSeenSec"`
	lastSeen    time.Time
}

// radioInfo tracks one dongle's health.
type radioInfo struct {
	Device       int    `json:"device"`
	FreqHz       uint64 `json:"freqHz"`
	Bursts       int    `json:"bursts"`
	LastBurstSec int64  `json:"lastBurstSec"` // -1 = never
	lastBurst    time.Time
}

type statsCollector struct {
	mu     sync.Mutex
	start  time.Time
	mode   string
	gps    *gpsReader
	radios []*radioInfo
	meters map[string]*meterStat
	total  int
	recent []recentRow
}

func newStatsCollector(mode string, gps *gpsReader) *statsCollector {
	return &statsCollector{
		start:  time.Now(),
		mode:   mode,
		gps:    gps,
		meters: make(map[string]*meterStat),
	}
}

func (s *statsCollector) addRadio(device int, freqHz uint64) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.radios = append(s.radios, &radioInfo{Device: device, FreqHz: freqHz})
}

const recentKeep = 15

func (s *statsCollector) record(row recentRow) {
	s.mu.Lock()
	defer s.mu.Unlock()

	now := time.Now()
	s.total++

	for _, r := range s.radios {
		if r.Device == row.Radio {
			r.Bursts++
			r.lastBurst = now
		}
	}

	if row.ID != "" {
		m := s.meters[row.ID]
		if m == nil {
			m = &meterStat{ID: row.ID}
			s.meters[row.ID] = m
		}
		m.Bursts++
		m.lastSeen = now
		m.LastRSSI = row.RSSI
		if row.Leak > m.Leak {
			m.Leak = row.Leak
		}
		if row.LeakNow > m.LeakNow {
			m.LeakNow = row.LeakNow
		}
	}

	s.recent = append(s.recent, row)
	if len(s.recent) > recentKeep {
		s.recent = s.recent[len(s.recent)-recentKeep:]
	}
}

// statsSnapshot is the dashboard's polled state.
type statsSnapshot struct {
	Mode          string      `json:"mode"`
	UptimeSec     int64       `json:"uptimeSec"`
	TotalBursts   int         `json:"totalBursts"`
	UniqueMeters  int         `json:"uniqueMeters"`
	LeakingMeters int         `json:"leakingMeters"`
	Radios        []radioInfo `json:"radios"`
	Leaking       []meterStat `json:"leaking"`
	Recent        []recentRow `json:"recent"`
	GPSEnabled    bool        `json:"gpsEnabled"`
	GPS           gpsStatus   `json:"gps"`
}

func (s *statsCollector) snapshot() statsSnapshot {
	s.mu.Lock()
	defer s.mu.Unlock()

	now := time.Now()
	snap := statsSnapshot{
		Mode:         s.mode,
		UptimeSec:    int64(now.Sub(s.start).Seconds()),
		TotalBursts:  s.total,
		UniqueMeters: len(s.meters),
		GPSEnabled:   s.gps != nil,
	}

	for _, r := range s.radios {
		cp := *r
		cp.LastBurstSec = -1
		if !r.lastBurst.IsZero() {
			cp.LastBurstSec = int64(now.Sub(r.lastBurst).Seconds())
		}
		snap.Radios = append(snap.Radios, cp)
	}

	for _, m := range s.meters {
		if m.Leak == 0 && m.LeakNow == 0 {
			continue
		}
		snap.LeakingMeters++
		cp := *m
		cp.LastSeenSec = int64(now.Sub(m.lastSeen).Seconds())
		snap.Leaking = append(snap.Leaking, cp)
	}
	sort.Slice(snap.Leaking, func(i, j int) bool {
		a, b := snap.Leaking[i], snap.Leaking[j]
		if a.LeakNow != b.LeakNow {
			return a.LeakNow > b.LeakNow
		}
		if a.Leak != b.Leak {
			return a.Leak > b.Leak
		}
		return a.ID < b.ID
	})
	if len(snap.Leaking) > 100 {
		snap.Leaking = snap.Leaking[:100]
	}

	// Recent feed newest-first.
	for i := len(s.recent) - 1; i >= 0; i-- {
		snap.Recent = append(snap.Recent, s.recent[i])
	}

	if s.gps != nil {
		snap.GPS = s.gps.statusSnapshot(gpsMaxAge)
	}

	return snap
}

// statsLine wraps an instance's line handler, extracting meter fields from
// rtlamr's CSV stream (header-driven, so it works for any message type)
// before passing the line on unchanged.
type statsLine struct {
	stats  *statsCollector
	radio  int
	colIdx map[string]int
	next   func(string)
}

func (t *statsLine) handleLine(line string) {
	fields := strings.Split(line, ",")

	if len(fields) > 0 && fields[0] == "Time" {
		t.colIdx = make(map[string]int, len(fields))
		for idx, name := range fields {
			t.colIdx[name] = idx
		}
	} else if t.colIdx != nil {
		get := func(name string) string {
			if idx, ok := t.colIdx[name]; ok && idx < len(fields) {
				return fields[idx]
			}
			return ""
		}
		atoi := func(v string) int {
			n, _ := strconv.Atoi(v)
			return n
		}

		id := get("ID")
		if id == "" {
			id = get("ERTSerialNumber") // idm/netidm
		}
		if id == "" {
			id = get("EndpointID") // scm+
		}

		t.stats.record(recentRow{
			Time:    get("Time"),
			Radio:   t.radio,
			ID:      id,
			RSSI:    get("RSSI"),
			Leak:    atoi(get("Leak")),
			LeakNow: atoi(get("LeakNow")),
		})
	}

	t.next(line)
}
