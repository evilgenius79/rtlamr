[![AGPLv3 License](https://img.shields.io/badge/license-AGPLv3-blue.svg?style=flat)](http://choosealicense.com/licenses/agpl-3.0/)

# rtlamr — Water Meter Leak Survey Edition

An rtl-sdr receiver for smart meters in the 900MHz ISM band, extended for
**finding water leaks across a neighborhood**. This is a fork of
[bemasher/rtlamr](https://github.com/bemasher/rtlamr) (by Douglas Hall) that
adds per-burst signal strength, a GPS-stamped mobile survey mode, a one-click
scan launcher with a live browser dashboard, and a series of decoder bug
fixes and performance improvements.

Neptune R900 water meter transmitters broadcast more than consumption: they
carry **leak flags** the meters compute themselves. Drive or park with an
rtl-sdr dongle and this tool logs every meter it hears — serial number, leak
status, backflow, consumption, signal strength, and GPS position — into CSV
files ready for mapping and reporting.

---

## What's in this fork

- **Per-burst RSSI and SNR** measured inside the R900 decoder from each
  burst's own carrier-on/off chips — the field that makes locating meters
  from drive-by data possible (upstream rtlamr does not output signal
  strength).
- **`rtlamrscan`**, a one-click launcher: auto-detects dongles, starts
  `rtl_tcp` for each, decodes, merges output, and cleans everything up when
  its window closes.
- **Mobile survey mode** with USB GPS (NMEA) support: every burst stamped
  with position, speed, and heading; no deduplication (every burst is a
  position+signal sample); rows flushed to disk as they decode.
- **Live dashboard** served from the binary at `http://127.0.0.1:8321`:
  meters found, leak-flagged meters, per-radio health, GPS fix status, and
  a live burst feed.
- **CSV header rows** on all output, so files open straight into a
  spreadsheet and sort by the leak columns.
- **Fixes over upstream**: a build that didn't compile at HEAD, tuner gain
  flags that were silently dropped (gain control matters for RSSI), a data
  race in IDM messages, swallowed CSV write errors, and a large R900 CPU
  optimization (the matched filter now runs only on blocks that contain a
  candidate packet).
- **Tests** where there were none: an end-to-end R900 decode test against a
  synthesized waveform (including Reed-Solomon parity), RSSI verification,
  and NMEA parser tests.

## Quick start (Windows, drive-by survey)

1. Make a folder containing:
   - `rtlamr.exe` and `rtlamrscan.exe` — build from this repo (see
     [Building](#building)) or grab prebuilt copies if you have them.
   - `rtl_tcp.exe` and its DLLs from the
     [rtl-sdr Windows package](https://ftp.osmocom.org/binaries/windows/rtl-sdr/).
2. One-time hardware setup:
   - Install the dongle's WinUSB driver with [Zadig](https://zadig.akeo.ie/).
   - Plug in the GPS puck and note its COM port (Device Manager → Ports).
3. Preflight the GPS (expect no fix indoors — it needs sky view):

   ```
   rtlamrscan -gps COM13 -gpstest
   ```

4. Drive:

   ```
   rtlamrscan -gps COM13
   ```

A browser tab opens with the live dashboard. Green **GPS FIX** banner plus a
climbing meter count means the survey is working. Close the window (or
Ctrl+C) to stop; everything shuts down cleanly.

## Scan modes

| Mode | What it does |
|---|---|
| `survey` (default) | Mobile drive-by survey. Decodes R900 water meters, stamps every burst with GPS position and RSSI/SNR, keeps every burst (no dedup), fixed tuner gain on all radios so RSSI is comparable. |
| `-mode water` | Stationary leak scan. Decodes R900 with duplicate readings suppressed — park it and let it accumulate the neighborhood. |
| `-mode mixed` | Dongle 1 decodes R900 water meters; dongle 2 decodes electric/gas (SCM/SCM+/IDM). |

One dongle is the default. `-dongles 2` or `-dongles 3` (or `-dongles 0` to
auto-detect) spreads radios across the 902–928MHz band the meters hop over —
906.0/912.38/918.5MHz for three, a tiled pair around 912.38MHz for two —
roughly multiplying the catch rate by the radio count. R900 meters hop the
whole band while each dongle hears a ~2.4MHz slice, so expect to catch each
meter intermittently and let time (or more passes) fill in the rest.

Other useful flags: `-duration 2h` (stop after a fixed time), `-gain 40`
(fixed tuner gain in dB), `-freqs 906000000,912380000,918500000` (override
centers), `-outdir C:\scans`, `-http off` (disable the dashboard),
`-gpsbaud 460800` (force a GPS baud instead of auto-detecting).

## Output files

Each survey run writes three files sharing one timestamp:

- **`survey_<ts>.csv`** — one row per decoded burst, merged across radios:

  ```
  Time,Radio,Lat,Lon,FixQuality,NumSats,HDOP,RSSI,SNR,ID,BackFlow,Consumption,Leak,LeakNow,
  FreqHz,Unkn1,NoUse,Unkn3,AltitudeM,SpeedKmh,Course,GPSAgeSec
  ```

  Position columns are blank (never 0,0) when there's no fresh GPS fix.
  Altitude, ground speed, and course come from the puck's GGA/RMC
  sentences — bursts caught while moving slowly are tighter location
  evidence, and the speed column lets downstream tools weight them.

- **`scan_<ts>_info.txt`** — the exact run configuration: command line,
  per-dongle center frequencies, fixed gain, GPS port. This is what makes
  RSSI values interpretable and runs reproducible later.

- **`scan_<ts>_log.txt`** — the full run log: GPS fix acquired/lost events,
  radio restarts, `rtl_tcp` output. Any gap in the data has its explanation
  here.

Stationary `water` mode writes `r900_<ts>.csv` (rtlamr's standard R900
columns plus RSSI/SNR) with the same sidecar files.

## R900 fields and leak flags

Field meanings are community reverse engineering — Neptune does not publish
the format — so verify against a meter you control before relying on them:

- **ID** — meter serial number (printed on the meter's register).
- **Consumption** — total consumption. Some meters encode this as
  binary-coded decimal; use `-msgtype=r900bcd` with plain `rtlamr` if
  values look wrong.
- **LeakNow** — leak status for the past 24 hours: 0 = none,
  1 = intermittent leak, 2 = continuous leak (matches Neptune E-Coder
  leak flags). *This is the headline column for a leak report.*
- **Leak** — binned count of days a leak condition was flagged over roughly
  the past 35 days.
- **BackFlow** — backflow detected during the past ~35 days: 0 = none,
  1 = low, 2 = high.
- **NoUse** — binned count of days with no usage (useful for separating
  vacant properties from leaks).
- **RSSI / SNR** — per-burst signal strength and signal-to-noise ratio in
  uncalibrated dB relative to receiver full scale. Comparable between
  receptions on the same hardware at the same **fixed** gain — automatic
  gain control makes RSSI meaningless, which is why survey mode pins it.
- **Unkn1 / Unkn3** — unknown fields, logged for completeness.

## Hardware tips for surveys

- **Same antenna type on every radio** — RSSI is only comparable when the
  front ends match. Identical mag-mount 915MHz omnis work well.
- **Powered USB hub** — each dongle draws ~300mA.
- **Give dongles unique USB serials** once (`rtl_eeprom -d 0 -s 1`, one
  plugged in at a time — many ship as `00000001`). Note `rtl_tcp` addresses
  devices by index; the index→serial mapping for each run is visible in the
  scan log.
- Drive slowly and cover streets in both directions when possible; more
  passes mean more samples per meter and tighter location estimates.

## Standalone decoder (`rtlamr`)

The underlying decoder works exactly like upstream rtlamr and can be used on
its own against any `rtl_tcp` instance:

```bash
rtl_tcp &
rtlamr -msgtype=r900 -format=csv -unique=true | tee r900.csv
```

Supported message types: `scm`, `scm+`, `idm`, `netidm`, `r900`, `r900bcd`
(see the [upstream wiki](https://github.com/bemasher/rtlamr/wiki/Configuration)
for full configuration). CSV output includes a header row. On
CPU-constrained devices, `-symbollength=32` roughly halves the sample rate
at some cost in sensitivity.

rtlamr only needs a TCP connection to `rtl_tcp`, so it runs anywhere Go
runs — including Android via [Termux](https://termux.dev/) with an
rtl_tcp-compatible SDR driver app.

## Building

Go ≥ 1.21. From the repo root:

```bash
go build .                  # rtlamr (the decoder)
go build ./cmd/rtlamrscan   # rtlamrscan (the launcher + dashboard)
```

Cross-compile for Windows from anywhere:

```bash
GOOS=windows GOARCH=amd64 CGO_ENABLED=0 go build -trimpath -ldflags="-s -w" -o rtlamr.exe .
GOOS=windows GOARCH=amd64 CGO_ENABLED=0 go build -trimpath -ldflags="-s -w" -o rtlamrscan.exe ./cmd/rtlamrscan
```

Run the tests (including the end-to-end R900 decode test):

```bash
go test ./...
```

## Compatibility

Any ERT-capable smart meter should work for the SCM/SCM+/IDM types; R900
covers Neptune R900-family water meter transmitters. See upstream's
[compatible meters table](https://github.com/bemasher/rtlamr/blob/master/meters.md)
and look for the FCC ID label on the meter itself.

## Ethics

This tool reads broadcasts that meters transmit in the clear, but position
and consumption data about your neighbors deserves care. Use it for good:
finding leaks the utility is missing, tracking your own usage, research with
anonymized data. Do not use it to profile specific people's living patterns.
When reporting leaks to a utility, share what's needed (meter ID, location,
leak flags) — that's the point — and keep the rest to yourself.

## Credits and License

Built on [rtlamr](https://github.com/bemasher/rtlamr), copyright Douglas
Hall, licensed under the GNU Affero General Public License v3.0. This fork
remains under [AGPLv3](http://choosealicense.com/licenses/agpl-3.0/): source
must be made available when distributing the software (including network
use), changes must be indicated, and no warranty is provided — see the
LICENSE file for the full terms.
