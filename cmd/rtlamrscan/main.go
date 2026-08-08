// rtlamrscan is a one-click launcher for neighborhood meter scans. It starts
// one rtl_tcp instance per detected dongle, runs an rtlamr decoder against
// each, and writes timestamped CSV files. Closing the window (or Ctrl+C)
// shuts everything down.
//
// The rtl_tcp and rtlamr binaries are expected in the same directory as this
// executable (or on PATH).
package main

import (
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
	msgType0 = flag.String("msgtype", "r900", "message types for the first dongle")
)

const maxDongles = 2

// Message types for each dongle: the first scans water meters (R900), the
// second picks up the other ERT meter types.
var dongleRoles = [maxDongles]struct{ msgType, filePrefix string }{
	{"", "r900"}, // msgType filled from the -msgtype flag
	{"scm,scm+,idm", "ert"},
}

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
		fmt.Fprintf(os.Stderr, "%-7s %s\n", w.prefix, bytes.TrimRight(w.buf[:idx], "\r"))
		stderrMu.Unlock()
		w.buf = w.buf[idx+1:]
	}
	return len(p), nil
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

// runDecoder runs rtlamr against an rtl_tcp port, writing CSV to a file.
// It restarts the decoder a few times if it dies early (e.g. rtl_tcp wasn't
// accepting connections yet).
func runDecoder(ctx context.Context, bin string, port int, msgType, outPath string) error {
	out, err := os.OpenFile(outPath, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0644)
	if err != nil {
		return err
	}
	defer out.Close()

	label := fmt.Sprintf("[%s]", msgType)
	if idx := bytes.IndexByte([]byte(msgType), ','); idx > 0 {
		label = fmt.Sprintf("[%s..]", msgType[:idx])
	}

	args := []string{
		"-server", fmt.Sprintf("127.0.0.1:%d", port),
		"-msgtype", msgType,
		"-format", "csv",
		"-unique", "true",
	}
	if *duration > 0 {
		args = append(args, "-duration", duration.String())
	}

	const maxAttempts = 4
	for attempt := 1; ; attempt++ {
		start := time.Now()

		cmd := exec.CommandContext(ctx, bin, args...)
		cmd.Stdout = out
		cmd.Stderr = &prefixWriter{prefix: label}

		err := cmd.Run()

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
		log.Printf("%s rtlamr exited (%v), restarting (attempt %d/%d)", label, err, attempt+1, maxAttempts)
	}
}

func main() {
	log.SetFlags(log.Ltime)
	flag.Parse()

	if *dongles < 0 || *dongles > maxDongles {
		log.Fatalf("-dongles must be between 0 and %d", maxDongles)
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

	type instance struct {
		device, port int
		msgType      string
		outPath      string
	}
	var instances []instance

	timestamp := time.Now().Format("20060102_150405")

	for device := 0; device < want; device++ {
		port := *basePort + device
		_, err := startRtlTCP(ctx, rtlTCPBin, device, port)
		if err != nil {
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

		role := dongleRoles[device]
		msgType := role.msgType
		if device == 0 {
			msgType = *msgType0
		}

		instances = append(instances, instance{
			device:  device,
			port:    port,
			msgType: msgType,
			outPath: filepath.Join(*outDir, fmt.Sprintf("%s_%s.csv", role.filePrefix, timestamp)),
		})
	}

	log.Printf("found %d dongle(s)", len(instances))

	var wg sync.WaitGroup
	for _, inst := range instances {
		log.Printf("dongle %d: decoding %s -> %s", inst.device, inst.msgType, inst.outPath)

		wg.Add(1)
		go func(inst instance) {
			defer wg.Done()
			if err := runDecoder(ctx, rtlamrBin, inst.port, inst.msgType, inst.outPath); err != nil && ctx.Err() == nil {
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
