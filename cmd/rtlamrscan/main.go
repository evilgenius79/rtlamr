// rtlamrscan is a one-click launcher for neighborhood meter scans. It starts
// one rtl_tcp instance per detected dongle, runs an rtlamr decoder against
// each, and writes timestamped CSV files. Closing the window (or Ctrl+C)
// shuts everything down.
//
// In the default "water" mode every dongle decodes R900 water meters. R900
// transmitters hop across the 902-928MHz band while each dongle only
// captures a ~2.4MHz slice, so with two dongles the launcher tunes them to
// adjacent slices (one below 912.4MHz, one above) and merges both decoders
// into a single CSV, roughly doubling the number of transmissions caught.
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
	"sync"
	"time"
)

var (
	dongles  = flag.Int("dongles", 0, "number of dongles to use, 0 to auto-detect (max 2)")
	duration = flag.Duration("duration", 0, "how long to scan, 0 to run until closed, ex. 1h30m")
	outDir   = flag.String("outdir", ".", "directory to write csv files to")
	basePort = flag.Int("baseport", 12340, "first tcp port used for rtl_tcp instances")
	mode     = flag.String("mode", "water", "water: all dongles scan r900 water meters splitting bandwidth; mixed: dongle 1 water, dongle 2 electric/gas")
	freqLow  = flag.Uint("freqlow", 911200000, "center frequency of the lower r900 slice when two dongles scan water")
	freqHigh = flag.Uint("freqhigh", 913560000, "center frequency of the upper r900 slice when two dongles scan water")
)

const maxDongles = 2

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
// single file, writing the header row only once (repeated header lines,
// e.g. from a second dongle or a decoder restart, are dropped).
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
	msgType      string
	centerFreq   uint // 0 leaves rtlamr's default for the message type
	label        string
	sink         *csvSink
	outPath      string
}

// runDecoder runs rtlamr against an rtl_tcp port, streaming CSV lines into
// the instance's sink. It restarts the decoder a few times if it dies early
// (e.g. rtl_tcp wasn't accepting connections yet).
func runDecoder(ctx context.Context, bin string, inst instance) error {
	args := []string{
		"-server", fmt.Sprintf("127.0.0.1:%d", inst.port),
		"-msgtype", inst.msgType,
		"-format", "csv",
		"-unique", "true",
	}
	if inst.centerFreq > 0 {
		args = append(args, "-centerfreq", fmt.Sprintf("%d", inst.centerFreq))
	}
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
			inst.sink.writeLine(scanner.Text())
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

func main() {
	log.SetFlags(log.Ltime)
	flag.Parse()

	if *dongles < 0 || *dongles > maxDongles {
		log.Fatalf("-dongles must be between 0 and %d", maxDongles)
	}
	if *mode != "water" && *mode != "mixed" {
		log.Fatalf("-mode must be water or mixed")
	}

	rtlTCPBin, err := findBinary("rtl_tcp")
	if err != nil {
		log.Printf("error: %v", err)
		log.Printf("download the rtl-sdr windows package from https://ftp.osmocom.org/binaries/windows/rtl-sdr/")
		log.Printf("and put rtl_tcp (and its dlls) in the same folder as this program")
		waitForEnter()
		os.Exit(1)
	}

	rtlamrBin, err := findBinary("rtlamr")
	if err != nil {
		log.Printf("error: %v", err)
		waitForEnter()
		os.Exit(1)
	}

	// Kill all child processes when this process exits, however it exits.
	killChildrenOnExit()

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt)
	defer stop()

	// Start rtl_tcp for each dongle. In auto-detect mode, keep going until a
	// device index fails to open.
	want := *dongles
	if want == 0 {
		want = maxDongles
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
			waitForEnter()
			os.Exit(1)
		}
		found++
	}

	log.Printf("found %d dongle(s)", found)

	timestamp := time.Now().Format("20060102_150405")
	waterPath := filepath.Join(*outDir, fmt.Sprintf("r900_%s.csv", timestamp))

	waterSink, err := newCSVSink(waterPath)
	if err != nil {
		log.Printf("error: %v", err)
		waitForEnter()
		os.Exit(1)
	}
	defer waterSink.Close()

	var instances []instance

	// The first dongle always scans water meters. With a single dongle it
	// stays on rtlamr's default r900 frequency; when a second dongle shares
	// the load in water mode, the two are tuned to adjacent slices.
	first := instance{
		device: 0, port: *basePort,
		msgType: "r900", label: "[r900]", sink: waterSink, outPath: waterPath,
	}

	switch {
	case *mode == "water" && found >= 2:
		first.centerFreq = *freqLow
		first.label = "[r900-lo]"
		instances = append(instances, first, instance{
			device: 1, port: *basePort + 1,
			msgType: "r900", centerFreq: *freqHigh,
			label: "[r900-hi]", sink: waterSink, outPath: waterPath,
		})
		log.Printf("water mode: dongle 0 centered at %dHz, dongle 1 at %dHz", *freqLow, *freqHigh)

	case *mode == "mixed" && found >= 2:
		ertPath := filepath.Join(*outDir, fmt.Sprintf("ert_%s.csv", timestamp))
		ertSink, err := newCSVSink(ertPath)
		if err != nil {
			log.Printf("error: %v", err)
			waitForEnter()
			os.Exit(1)
		}
		defer ertSink.Close()

		instances = append(instances, first, instance{
			device: 1, port: *basePort + 1,
			msgType: "scm,scm+,idm", label: "[ert]", sink: ertSink, outPath: ertPath,
		})

	default:
		instances = append(instances, first)
	}

	var wg sync.WaitGroup
	for _, inst := range instances {
		log.Printf("dongle %d: decoding %s -> %s", inst.device, inst.msgType, inst.outPath)

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
