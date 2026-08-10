// rtlamrscan is a one-click launcher for neighborhood meter scans. It starts
// one rtl_tcp instance per detected dongle, runs an rtlamr decoder against
// each, and writes timestamped CSV files. Closing the window (or Ctrl+C)
// shuts everything down.
//
// Modes:
//
//   - survey (default): mobile drive-by survey. Up to three dongles all
//     decode R900 water meters on centers spread across the 902-928MHz hop
//     band, every burst is kept (no dedup) and stamped with the current GPS
//     position (-gps COM4) and per-burst RSSI/SNR, merged into one CSV in
//     the shape downstream mapping tools expect. All radios run at the same
//     fixed tuner gain so RSSI is comparable between them.
//
//   - water: stationary leak scan. All dongles decode R900 on adjacent
//     ~2.4MHz slices around 912.38MHz, duplicate readings suppressed.
//
//   - mixed: dongle 0 decodes R900, dongle 1 decodes SCM/SCM+/IDM.
//
// The rtl_tcp and rtlamr binaries are expected in the same directory as this
// executable (or on PATH).
package main

import (
	"bufio"
	"bytes"
	"context"
	"flag"
	"fmt"
	"log"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"time"
)

var (
	dongles  = flag.Int("dongles", 0, "number of dongles to use, 0 to auto-detect (max 3)")
	duration = flag.Duration("duration", 0, "how long to scan, 0 to run until closed, ex. 1h30m")
	outDir   = flag.String("outdir", ".", "directory to write csv files to")
	basePort = flag.Int("baseport", 12340, "first tcp port used for rtl_tcp instances")
	mode     = flag.String("mode", "survey", "survey: mobile drive-by with gps+rssi; water: stationary r900 scan; mixed: dongle 1 water, dongle 2 electric/gas")
	freqs    = flag.String("freqs", "", "comma-separated center frequencies in Hz, one per dongle (default: per-mode plan)")
	gain     = flag.Float64("gain", 40, "fixed tuner gain in dB for all radios (rssi comparability); applied in survey mode, or in other modes when set explicitly")
	gpsPort  = flag.String("gps", "", "serial port of NMEA GPS receiver, ex. COM4 (survey mode)")
	gpsBaud  = flag.Int("gpsbaud", 9600, "baud rate of the GPS serial port (usually 9600 or 4800)")
)

const maxDongles = 3

// gpsMaxAge is how stale the last GPS fix may be before rows are written
// with blank position columns instead.
const gpsMaxAge = 10 * time.Second

var stderrMu sync.Mutex

// prefixWriter labels each line of a child process's output so interleaved
// logs from multiple processes stay readable.
type prefixWriter struct {
	prefix string
	buf    []byte
}

func (w *prefixWriter) Write(p []byte) (int, error) {
	w.buf = append(w.buf, p...)
	for {
		idx := bytes.IndexByte(w.buf, '\n')
		if idx < 0 {
			break
		}
		stderrMu.Lock()
		fmt.Fprintf(os.Stderr, "%-10s %s\n", w.prefix, bytes.TrimRight(w.buf[:idx], "\r"))
		stderrMu.Unlock()
		w.buf = w.buf[idx+1:]
	}
	return len(p), nil
}

// csvSink serializes CSV lines from one or more decoder processes into a
// single file, writing each line to disk as it arrives. The first line
// written becomes the header; repeats of it (e.g. from a second dongle or a
// decoder restart) are dropped.
type csvSink struct {
	mu         sync.Mutex
	f          *os.File
	headerLine string
	gotHeader  bool
}

func newCSVSink(path string) (*csvSink, error) {
	f, err := os.OpenFile(path, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0644)
	if err != nil {
		return nil, err
	}
	return &csvSink{f: f}, nil
}

func (s *csvSink) writeLine(line string) {
	s.mu.Lock()
	defer s.mu.Unlock()

	if s.gotHeader && line == s.headerLine {
		return
	}
	if !s.gotHeader {
		s.headerLine = line
		s.gotHeader = true
	}
	fmt.Fprintln(s.f, line)
}

func (s *csvSink) Close() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.f.Close()
}

// surveyColumns is the row shape handed downstream for mapping.
var surveyColumns = []string{
	"Time", "Radio", "Lat", "Lon", "FixQuality", "NumSats", "HDOP",
	"RSSI", "SNR", "ID", "BackFlow", "Consumption", "Leak", "LeakNow",
}

// surveyMapper reshapes rtlamr CSV rows into the survey row shape, stamping
// each with the radio index and the GPS fix current at decode time.
type surveyMapper struct {
	radio  int
	gps    *gpsReader
	sink   *csvSink
	colIdx map[string]int
	warned bool
}

func (m *surveyMapper) handleLine(line string) {
	fields := strings.Split(line, ",")

	// Header rows arrive at decoder start and on decoder restarts.
	if len(fields) > 0 && fields[0] == "Time" {
		m.colIdx = make(map[string]int, len(fields))
		for idx, name := range fields {
			m.colIdx[name] = idx
		}
		for _, name := range []string{"ID", "BackFlow", "Consumption", "Leak", "LeakNow", "RSSI", "SNR"} {
			if _, ok := m.colIdx[name]; !ok && !m.warned {
				m.warned = true
				log.Printf("radio %d: decoder output is missing the %q column - is rtlamr up to date?", m.radio, name)
			}
		}
		return
	}
	if m.colIdx == nil {
		return // data before any header; can't interpret it
	}

	get := func(name string) string {
		if idx, ok := m.colIdx[name]; ok && idx < len(fields) {
			return fields[idx]
		}
		return ""
	}

	// Blank position columns when there's no usable fix; never 0,0.
	var lat, lon, quality, sats, hdop string
	if m.gps != nil {
		if fix, ok := m.gps.current(gpsMaxAge); ok {
			lat = strconv.FormatFloat(fix.Lat, 'f', 6, 64)
			lon = strconv.FormatFloat(fix.Lon, 'f', 6, 64)
			quality = strconv.Itoa(fix.Quality)
			sats = strconv.Itoa(fix.NumSats)
			hdop = strconv.FormatFloat(fix.HDOP, 'f', 1, 64)
		}
	}

	row := []string{
		get("Time"), strconv.Itoa(m.radio), lat, lon, quality, sats, hdop,
		get("RSSI"), get("SNR"), get("ID"), get("BackFlow"),
		get("Consumption"), get("Leak"), get("LeakNow"),
	}
	m.sink.writeLine(strings.Join(row, ","))
}

func exeName(name string) string {
	if runtime.GOOS == "windows" {
		return name + ".exe"
	}
	return name
}

// findBinary looks for name next to this executable first, then on PATH.
func findBinary(name string) (string, error) {
	name = exeName(name)

	if self, err := os.Executable(); err == nil {
		local := filepath.Join(filepath.Dir(self), name)
		if _, err := os.Stat(local); err == nil {
			return local, nil
		}
	}

	if path, err := exec.LookPath(name); err == nil {
		return path, nil
	}

	return "", fmt.Errorf("%s not found next to this program or on PATH", name)
}

// startRtlTCP starts rtl_tcp for the given device index and reports whether
// it is still running after a grace period. rtl_tcp exits almost immediately
// when the device index doesn't exist, which is how auto-detection works.
func startRtlTCP(ctx context.Context, bin string, device, port int) (*exec.Cmd, error) {
	cmd := exec.CommandContext(ctx, bin,
		"-d", fmt.Sprintf("%d", device),
		"-p", fmt.Sprintf("%d", port),
	)
	cmd.Stdout = &prefixWriter{prefix: fmt.Sprintf("[tcp%d]", device)}
	cmd.Stderr = &prefixWriter{prefix: fmt.Sprintf("[tcp%d]", device)}

	if err := cmd.Start(); err != nil {
		return nil, err
	}

	exited := make(chan error, 1)
	go func() { exited <- cmd.Wait() }()

	select {
	case err := <-exited:
		return nil, fmt.Errorf("rtl_tcp exited: %v (no dongle %d?)", err, device)
	case <-time.After(3 * time.Second):
		// Still running; assume the device opened successfully. Re-arm a
		// waiter so the process is reaped whenever it does exit.
		go func() { <-exited }()
		return cmd, nil
	}
}

type instance struct {
	device, port int
	args         []string // decoder args beyond -server/-format/-duration
	label        string
	handleLine   func(string)
	outPath      string
}

// runDecoder runs rtlamr against an rtl_tcp port, streaming each CSV line to
// the instance's line handler. It restarts the decoder a few times if it
// dies early (e.g. rtl_tcp wasn't accepting connections yet).
func runDecoder(ctx context.Context, bin string, inst instance) error {
	args := []string{
		"-server", fmt.Sprintf("127.0.0.1:%d", inst.port),
		"-format", "csv",
	}
	args = append(args, inst.args...)
	if *duration > 0 {
		args = append(args, "-duration", duration.String())
	}

	const maxAttempts = 4
	for attempt := 1; ; attempt++ {
		start := time.Now()

		cmd := exec.CommandContext(ctx, bin, args...)
		cmd.Stderr = &prefixWriter{prefix: inst.label}

		stdout, err := cmd.StdoutPipe()
		if err != nil {
			return err
		}

		if err := cmd.Start(); err != nil {
			return err
		}

		scanner := bufio.NewScanner(stdout)
		for scanner.Scan() {
			inst.handleLine(scanner.Text())
		}

		err = cmd.Wait()

		if ctx.Err() != nil || err == nil {
			return nil
		}
		if attempt >= maxAttempts {
			return fmt.Errorf("rtlamr kept failing: %v", err)
		}
		// Early deaths are usually rtl_tcp not ready yet; give it a moment.
		if time.Since(start) < 10*time.Second {
			time.Sleep(2 * time.Second)
		}
		log.Printf("%s rtlamr exited (%v), restarting (attempt %d/%d)", inst.label, err, attempt+1, maxAttempts)
	}
}

// freqPlan returns per-dongle r900 center frequencies. Single dongles stay
// on the proven default center. Two dongles tile adjacent slices around it.
// Three spread across the 902-928MHz hop band.
func freqPlan(mode string, count int) []uint {
	if *freqs != "" {
		var plan []uint
		for _, s := range strings.Split(*freqs, ",") {
			f, err := strconv.ParseUint(strings.TrimSpace(s), 10, 32)
			if err != nil {
				log.Fatalf("invalid -freqs value %q: frequencies are plain Hz, ex. 912380000", s)
			}
			plan = append(plan, uint(f))
		}
		if len(plan) < count {
			log.Fatalf("-freqs lists %d frequencies but %d dongles are in use", len(plan), count)
		}
		return plan[:count]
	}

	switch count {
	case 1:
		return []uint{0} // rtlamr's default r900 center (912.38MHz)
	case 2:
		return []uint{911200000, 913560000}
	default:
		return []uint{906000000, 912380000, 918500000}
	}
}

func fatal(format string, args ...interface{}) {
	log.Printf(format, args...)
	waitForEnter()
	os.Exit(1)
}

func main() {
	log.SetFlags(log.Ltime)
	flag.Parse()

	if *dongles < 0 || *dongles > maxDongles {
		log.Fatalf("-dongles must be between 0 and %d", maxDongles)
	}
	switch *mode {
	case "survey", "water", "mixed":
	default:
		log.Fatalf("-mode must be survey, water or mixed")
	}

	gainSet := *mode == "survey"
	flag.Visit(func(f *flag.Flag) {
		if f.Name == "gain" {
			gainSet = true
		}
	})

	rtlTCPBin, err := findBinary("rtl_tcp")
	if err != nil {
		log.Printf("error: %v", err)
		log.Printf("download the rtl-sdr windows package from https://ftp.osmocom.org/binaries/windows/rtl-sdr/")
		fatal("and put rtl_tcp (and its dlls) in the same folder as this program")
	}

	rtlamrBin, err := findBinary("rtlamr")
	if err != nil {
		fatal("error: %v", err)
	}

	// Kill all child processes when this process exits, however it exits.
	killChildrenOnExit()

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt)
	defer stop()

	// GPS first: a typo'd COM port should fail before radios spin up.
	var gps *gpsReader
	if *gpsPort != "" {
		gps, err = startGPS(ctx, *gpsPort, *gpsBaud)
		if err != nil {
			fatal("error opening gps port %s: %v", *gpsPort, err)
		}
		log.Printf("gps: reading NMEA from %s at %d baud", *gpsPort, *gpsBaud)
	} else if *mode == "survey" {
		log.Printf("warning: no -gps port given, position columns will be blank")
	}

	// Start rtl_tcp for each dongle. In auto-detect mode, keep going until a
	// device index fails to open.
	want := *dongles
	if want == 0 {
		want = maxDongles
	}
	if *mode != "survey" && want > 2 {
		want = 2
	}

	found := 0
	for device := 0; device < want; device++ {
		if _, err := startRtlTCP(ctx, rtlTCPBin, device, *basePort+device); err != nil {
			if *dongles == 0 && device > 0 {
				break // auto-detect: ran out of dongles
			}
			log.Printf("error: %v", err)
			if device == 0 {
				log.Printf("is the dongle plugged in and its driver installed (zadig)?")
			}
			fatal("giving up")
		}
		found++
	}

	log.Printf("found %d dongle(s)", found)

	timestamp := time.Now().Format("20060102_150405")

	gainArgs := func() []string {
		if !gainSet {
			return nil
		}
		return []string{"-tunergain", strconv.FormatFloat(*gain, 'f', 1, 64)}
	}
	freqArgs := func(freq uint) []string {
		if freq == 0 {
			return nil
		}
		return []string{"-centerfreq", strconv.FormatUint(uint64(freq), 10)}
	}

	var instances []instance
	var sinks []*csvSink

	switch *mode {
	case "survey":
		outPath := filepath.Join(*outDir, fmt.Sprintf("survey_%s.csv", timestamp))
		sink, err := newCSVSink(outPath)
		if err != nil {
			fatal("error: %v", err)
		}
		sinks = append(sinks, sink)
		sink.writeLine(strings.Join(surveyColumns, ","))

		if gainSet {
			log.Printf("survey: all radios at fixed %.1fdB tuner gain for rssi comparability", *gain)
		}

		plan := freqPlan(*mode, found)
		for device := 0; device < found; device++ {
			mapper := &surveyMapper{radio: device, gps: gps, sink: sink}
			args := []string{"-msgtype", "r900"}
			args = append(args, freqArgs(plan[device])...)
			args = append(args, gainArgs()...)

			instances = append(instances, instance{
				device: device, port: *basePort + device,
				args:       args,
				label:      fmt.Sprintf("[radio%d]", device),
				handleLine: mapper.handleLine,
				outPath:    outPath,
			})
		}

	case "water":
		outPath := filepath.Join(*outDir, fmt.Sprintf("r900_%s.csv", timestamp))
		sink, err := newCSVSink(outPath)
		if err != nil {
			fatal("error: %v", err)
		}
		sinks = append(sinks, sink)

		plan := freqPlan(*mode, found)
		for device := 0; device < found; device++ {
			args := []string{"-msgtype", "r900", "-unique", "true"}
			args = append(args, freqArgs(plan[device])...)
			args = append(args, gainArgs()...)

			instances = append(instances, instance{
				device: device, port: *basePort + device,
				args:       args,
				label:      fmt.Sprintf("[r900-%d]", device),
				handleLine: sink.writeLine,
				outPath:    outPath,
			})
		}

	case "mixed":
		waterPath := filepath.Join(*outDir, fmt.Sprintf("r900_%s.csv", timestamp))
		waterSink, err := newCSVSink(waterPath)
		if err != nil {
			fatal("error: %v", err)
		}
		sinks = append(sinks, waterSink)

		args := []string{"-msgtype", "r900", "-unique", "true"}
		args = append(args, gainArgs()...)
		instances = append(instances, instance{
			device: 0, port: *basePort,
			args:       args,
			label:      "[r900]",
			handleLine: waterSink.writeLine,
			outPath:    waterPath,
		})

		if found >= 2 {
			ertPath := filepath.Join(*outDir, fmt.Sprintf("ert_%s.csv", timestamp))
			ertSink, err := newCSVSink(ertPath)
			if err != nil {
				fatal("error: %v", err)
			}
			sinks = append(sinks, ertSink)

			args := []string{"-msgtype", "scm,scm+,idm", "-unique", "true"}
			args = append(args, gainArgs()...)
			instances = append(instances, instance{
				device: 1, port: *basePort + 1,
				args:       args,
				label:      "[ert]",
				handleLine: ertSink.writeLine,
				outPath:    ertPath,
			})
		}
	}

	defer func() {
		for _, sink := range sinks {
			sink.Close()
		}
	}()

	var wg sync.WaitGroup
	for _, inst := range instances {
		log.Printf("dongle %d: %s -> %s", inst.device, strings.Join(inst.args, " "), inst.outPath)

		wg.Add(1)
		go func(inst instance) {
			defer wg.Done()
			if err := runDecoder(ctx, rtlamrBin, inst); err != nil && ctx.Err() == nil {
				log.Printf("dongle %d: %v", inst.device, err)
			}
		}(inst)
	}

	done := make(chan struct{})
	go func() { wg.Wait(); close(done) }()

	log.Printf("scanning... close this window or press Ctrl+C to stop")

	select {
	case <-ctx.Done():
		log.Printf("stopping...")
	case <-done:
		log.Printf("all decoders finished")
	}

	stop()

	// Give children a moment to be reaped after context cancellation.
	select {
	case <-done:
	case <-time.After(5 * time.Second):
	}
}

// waitForEnter keeps the console window open on error so double-click users
// can read the message before the window disappears.
func waitForEnter() {
	if runtime.GOOS != "windows" {
		return
	}
	fmt.Fprintln(os.Stderr, "press enter to close...")
	fmt.Scanln()
}
