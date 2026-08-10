[![AGPLv3 License](https://img.shields.io/badge/license-AGPLv3-blue.svg?style=flat)](http://choosealicense.com/licenses/agpl-3.0/)

### Purpose

Utilities often use "smart meters" to optimize their residential meter reading infrastructure. Smart meters transmit consumption information in the various ISM bands allowing utilities to simply send readers driving through neighborhoods to collect commodity consumption information. One protocol in particular: Encoder Receiver Transmitter by Itron is fairly straight forward to decode and operates in the 900MHz ISM band, well within the tunable range of inexpensive rtl-sdr dongles.

This project is a software defined radio receiver for these messages. We make use of an inexpensive rtl-sdr dongle to allow users to non-invasively record and analyze the commodity consumption of their household.

There's now experimental support for data collection and aggregation with [rtlamr-collect](https://github.com/bemasher/rtlamr-collect)!

### Requirements

- GoLang >=1.21 (Go build environment setup guide: http://golang.org/doc/code.html)
- rtl-sdr
  - Windows: [pre-built binaries](https://ftp.osmocom.org/binaries/windows/rtl-sdr/)
  - Linux: [source and build instructions](http://sdr.osmocom.org/trac/wiki/rtl-sdr)

### Install
To install rtlamr, run the following:  
- For Go versions >= 1.16: 
```bash
go install github.com/bemasher/rtlamr@latest
```
- Or, for older versions of Go: 
```bash
go get github.com/bemasher/rtlamr
```

The command above will add the binary to `$HOME/go/bin/`, or if `$GOPATH` is set, `$GOPATH/bin/`.

To run the rtlamr binary from any directory, ensure the directory containing the binary is in your `PATH` ([more info](https://superuser.com/questions/284342/what-are-path-and-other-environment-variables-and-how-can-i-set-or-use-them)).

### Usage

See the wiki page [Configuration](https://github.com/bemasher/rtlamr/wiki/Configuration) for details on configuring rtlamr.

Running the receiver is as simple as starting an [`rtl_tcp`](https://osmocom.org/projects/rtl-sdr/wiki/Rtl-sdr) instance and then starting the receiver:

```bash
# Terminal A
$ rtl_tcp

# Terminal B
$ rtlamr
```

The animation below shows an example of starting rtlamr along with the successful capture of an ERT message.
![Animation of output when starting rtlamr](assets/run_rtlamr.gif)  

---

If you want to run the spectrum server on a different machine than the receiver you'll need to specify an address to listen on with the `-a` flag for `rtl_tcp`, and the `-server` flag for `rtlamr`.

### Message Types

The following message types are supported by rtlamr:

- **scm**: Standard Consumption Message. Simple packet that reports total consumption.
- **scm+**: Similar to SCM, allows greater precision and longer meter ID's.
- **idm**: Interval Data Message. Provides differential consumption data for previous 47 intervals at 5 minutes per interval.
- **netidm**: Similar to IDM, except net meters (type 8) have different internal packet structure, number of intervals and precision. Also reports total power production.
- **r900**: Message type used by Neptune R900 transmitters, provides total consumption and leak flags.
- **r900bcd**: Some Neptune R900 meters report consumption as a binary-coded digits.

### R900 Water Meters and Leak Flags

Neptune R900 messages carry more than total consumption. The format is not
published by the vendor, so field meanings below come from community reverse
engineering — verify against a meter you control before relying on them:

- **ID**: Meter serial number (printed on the meter's face/register).
- **Consumption**: Total consumption. Some meters encode this as binary-coded
  decimal; use `-msgtype=r900bcd` if consumption values from `r900` look wrong.
- **NoUse**: Binned count of days with no usage over roughly the past 35 days.
- **BackFlow**: Backflow detected during the past ~35 days: 0 = none,
  1 = low, 2 = high.
- **Leak**: Binned count of days a leak condition was flagged over roughly
  the past 35 days.
- **LeakNow**: Leak status for the past 24 hours: 0 = none, 1 = intermittent
  leak, 2 = continuous leak (matches Neptune E-Coder intermittent/continuous
  leak flags).
- **Unkn1**, **Unkn3**: Unknown fields.

To log every R900 reading to CSV (a header row is written automatically, and
`-unique=true` suppresses repeated identical readings from the same meter):

```bash
rtlamr -msgtype=r900 -format=csv -unique=true | tee r900.csv
```

The resulting file can be sorted by the leak columns in any spreadsheet, or
from a shell (`LeakNow` is column 11, `Leak` is column 10):

```bash
head -n1 r900.csv && tail -n +2 r900.csv | sort -t, -k11,11nr -k10,10nr
```

Note that R900 transmitters hop across many channels and rtlamr only listens
to part of that band, so leave it running for a while — expect to catch each
meter intermittently rather than on every transmission.

### R900 Signal Strength (RSSI/SNR)

Every decoded R900 message includes per-burst `RSSI` and `SNR` columns,
measured from the burst's own carrier-on vs carrier-off chips. Values are in
uncalibrated dB relative to receiver full scale: they are comparable between
receptions on the same hardware at the same **fixed** tuner gain (set
`-tunergain`; automatic gain makes RSSI meaningless), and between dongles of
the same model with identical antennas at the same gain.

### One-click Scanning: rtlamrscan

`cmd/rtlamrscan` builds a launcher that runs a whole scan from a single
program: it auto-detects up to three dongles, starts an `rtl_tcp` instance
for each, runs a decoder against each, and writes timestamped CSV files to
the current directory. Closing the window or pressing Ctrl+C stops
everything, including the `rtl_tcp` child processes.

```bash
go build ./cmd/rtlamrscan
```

Put the resulting binary in the same folder as `rtlamr` and `rtl_tcp` (on
Windows: `rtlamr.exe` and `rtl_tcp.exe` with its DLLs) and run it.

**survey mode (default)** is a mobile drive-by survey for locating meters:
up to three dongles all decode R900 on centers spread across the 902-928MHz
hop band (906.0/912.38/918.5MHz for three; a tiled pair around 912.38MHz for
two), every burst is kept as its own row (no dedup — each burst is a
position+signal sample), all radios run at the same fixed tuner gain
(`-gain`, default 40dB) so RSSI is comparable, and rows are appended to disk
as they decode. With `-gps COM13` a USB NMEA GPS puck stamps every row with
the position at decode time. The baud rate is auto-detected (460800 — common
on modern pucks — is tried first, then 9600/115200/38400/57600/4800; force
one with `-gpsbaud`), and sentences from any talker (`$GPGGA`, `$GNGGA`,
`$GLGGA`, ...) are accepted with checksum validation. Without a fix the
position columns are blank, never 0,0. Before a drive, verify the puck with:

```
rtlamrscan -gps COM13 -gpstest
```

which reports the detected baud, the talker ID, and live fix status (expect
no fix indoors — the antenna needs sky view). Output of a survey run is a
single merged `survey_<timestamp>.csv`:

```
Time,Radio,Lat,Lon,FixQuality,NumSats,HDOP,RSSI,SNR,ID,BackFlow,Consumption,Leak,LeakNow
```

For the drive itself: use the same antenna type on every radio (identical
mag-mount 915MHz omnis), fix each dongle's USB serial once so they stay
individually addressable (`rtl_eeprom -d 0 -s 1` etc., one plugged in at a
time — many ship as `00000001`), drive slowly, and cover streets from both
directions when possible. The `Radio` column records which dongle heard each
burst.

**water mode** (`-mode water`) is the stationary leak scan: all dongles
decode R900 on adjacent ~2.4MHz slices around 912.38MHz with duplicate
readings suppressed, merged into one `r900_<timestamp>.csv`.

**mixed mode** (`-mode mixed`) decodes R900 on dongle 1 and electric/gas
(SCM/SCM+/IDM) on dongle 2.

**Live dashboard.** While scanning, rtlamrscan serves a live stats page at
`http://127.0.0.1:8321` and opens it in the default browser: meters found,
leak-flagged meters, bursts captured, per-radio health (center frequency,
burst count, time since last decode), GPS fix status with position and
satellite count, and a feed of recent bursts. The page is served entirely
from the binary and works offline. Change the address with `-http`, or
disable with `-http off`.

Other flags: `-duration 1h` to stop after a fixed time, `-dongles N` to use
more than the default single dongle (`-dongles 0` auto-detects up to 3),
`-freqs 906000000,912380000,918500000` to override the per-dongle centers,
and `-outdir` to choose where CSVs are written.

### Low-power Devices and Multiple Dongles

rtlamr talks to the dongle only through `rtl_tcp`, so it runs anywhere Go
runs, including Android (e.g. in [Termux](https://termux.dev/)), with
`rtl_tcp` either on the same device or elsewhere on the network.

- To use two dongles on one host (a powered USB hub is recommended — each
  dongle draws around 300mA), run one `rtl_tcp` per dongle and point a
  separate rtlamr instance at each:

  ```bash
  rtl_tcp -d 0 -p 1234 &
  rtl_tcp -d 1 -p 1235 &
  rtlamr -server=127.0.0.1:1234 -msgtype=r900 -format=csv | tee r900.csv
  rtlamr -server=127.0.0.1:1235 -msgtype=scm,scm+,idm -format=csv | tee ert.csv
  ```

- On CPU-constrained devices, reduce the sample rate with `-symbollength`.
  The default of 72 samples per symbol needs ~2.4Msps; `-symbollength=32`
  processes less than half as many samples per second at some cost in
  sensitivity and channel coverage.

### Compatibility

Currently the only tested meter is the Itron C1SR and Itron 40G. However, the protocol is designed to be useful for several different commodities and should be capable of receiving messages from any ERT capable smart meter.

Check out the table of meters I've been compiling from various internet sources: [ERT Compatible Meters](https://github.com/bemasher/rtlamr/blob/master/meters.md)

User provided, but otherwise unverified compatible meters: [Google Sheets](https://docs.google.com/spreadsheets/d/1lTeHkk7rwFfq0joMWngrhnJA2nXAk4m82eApVaAKfhw/edit?usp=sharing)

Look for an FCC ID label on your meter, it should identify the two-digit commodity or endpoint type and the eight- or ten-digit endpoint ID of your meter: `## ########[##]`. Below are a few examples:

![Example FCC Label (1)](assets/fcc_label_01.png)
![Example FCC Label (2)](assets/fcc_label_02.png)
![Example FCC Label (3)](assets/fcc_label_03.png)

### Sensitivity

Using a NooElec NESDR Nano R820T with the provided antenna, I can reliably receive standard consumption messages from ~300 different meters and intermittently from another ~600 meters. These figures are calculated from the number of messages received during a 25 minute window. Reliably in this case means receiving at least 10 of the expected 12 messages and intermittently means 3-9 messages.

### Ethics

_Do not use this for malicious purposes._ If you do, I don't want to know about it, I am not and will not be responsible for your actions. However, if you find a clever non-evil use for this, by all means, share.

### Use Cases

These are a few examples of ways this tool could be used:

**Ethical**

- Track down stray appliances.
- Track power generated vs. power consumed.
- Find a water leak with rtlamr rather than from your bill.
- Optimize your thermostat to reduce energy consumption.
- Mass collection for research purposes. (_Please_ anonymize your data.)

**Unethical**

- Using data collected to determine living patterns of specific persons with the intent to act on this data, particularly without express permission to do so.

### License

The source of this project is licensed under Affero GPL v3.0. According to [http://choosealicense.com/licenses/agpl-3.0/](http://choosealicense.com/licenses/agpl-3.0/) you may:

#### Required:

- **Disclose Source:** Source code must be made available when distributing the software. In the case of LGPL, the source for the library (and not the entire program) must be made available.
- **License and copyright notice:** Include a copy of the license and copyright notice with the code.
- **Network Use is Distribution:** Users who interact with the software via network are given the right to receive a copy of the corresponding source code.
- **State Changes:** Indicate significant changes made to the code.

#### Permitted:

- **Commercial Use:** This software and derivatives may be used for commercial purposes.
- **Distribution:** You may distribute this software.
- **Modification:** This software may be modified.
- **Patent Grant:** This license provides an express grant of patent rights from the contributor to the recipient.
- **Private Use:** You may use and modify the software without distributing it.

#### Forbidden:

- **Hold Liable:** Software is provided without warranty and the software author/license owner cannot be held liable for damages.
- **Sublicensing:** You may not grant a sublicense to modify and distribute this software to third parties not included in the license.

### Feedback

If you have any questions, comments, feedback or bugs, please submit an issue.
