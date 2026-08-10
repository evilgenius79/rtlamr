package main

import (
	"encoding/json"
	"log"
	"net"
	"net/http"
	"os/exec"
	"runtime"
)

// startDashboard serves the live stats page and returns its URL.
func startDashboard(addr string, stats *statsCollector) (string, error) {
	mux := http.NewServeMux()

	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/" {
			http.NotFound(w, r)
			return
		}
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		w.Write([]byte(dashboardPage))
	})

	mux.HandleFunc("/stats", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(stats.snapshot())
	})

	ln, err := net.Listen("tcp", addr)
	if err != nil {
		return "", err
	}

	go func() {
		if err := http.Serve(ln, mux); err != nil {
			log.Printf("[web] dashboard server stopped: %v", err)
		}
	}()

	return "http://" + ln.Addr().String(), nil
}

// openBrowser opens the default browser at url; failures only log.
func openBrowser(url string) {
	var cmd *exec.Cmd
	switch runtime.GOOS {
	case "windows":
		cmd = exec.Command("rundll32", "url.dll,FileProtocolHandler", url)
	case "darwin":
		cmd = exec.Command("open", url)
	default:
		cmd = exec.Command("xdg-open", url)
	}
	if err := cmd.Start(); err != nil {
		log.Printf("[web] could not open browser: %v", err)
	}
}

// The dashboard commits to a single dark look (it's a monitoring console);
// all colors are painted explicitly and no external assets are used, so the
// page works fully offline.
const dashboardPage = `<!doctype html>
<html lang="en">
<head>
<meta charset="utf-8">
<meta name="viewport" content="width=device-width, initial-scale=1">
<title>rtlamr scan</title>
<style>
  :root {
    --surface: #1a1a19; --panel: #232321; --border: #34342f;
    --ink: #ffffff; --ink-2: #c3c2b7; --ink-3: #8f8e83;
    --good: #0ca30c; --warning: #fab219; --critical: #d03b3b;
  }
  * { box-sizing: border-box; margin: 0; }
  body {
    background: var(--surface); color: var(--ink-2);
    font: 14px/1.45 system-ui, "Segoe UI", sans-serif;
    padding: 16px; max-width: 1100px; margin: 0 auto;
  }
  header { display: flex; align-items: baseline; gap: 12px; margin-bottom: 14px; }
  h1 { font-size: 18px; color: var(--ink); font-weight: 600; }
  header .sub { color: var(--ink-3); font-size: 13px; }
  .banner {
    border: 1px solid var(--border); border-radius: 8px; padding: 10px 14px;
    margin-bottom: 14px; font-size: 15px; font-weight: 600; color: var(--ink);
    display: flex; gap: 10px; align-items: center; flex-wrap: wrap;
  }
  .banner .detail { font-weight: 400; color: var(--ink-2); font-size: 13px; }
  .dot { font-size: 16px; }
  .tiles { display: grid; grid-template-columns: repeat(auto-fit, minmax(150px, 1fr)); gap: 10px; margin-bottom: 14px; }
  .tile { background: var(--panel); border: 1px solid var(--border); border-radius: 8px; padding: 12px 14px; }
  .tile .label { font-size: 12px; color: var(--ink-3); text-transform: uppercase; letter-spacing: .04em; }
  .tile .value { font-size: 30px; font-weight: 650; color: var(--ink); font-variant-numeric: tabular-nums; margin-top: 2px; }
  .tile .value.crit { color: var(--critical); }
  .cols { display: grid; grid-template-columns: 1fr 1fr; gap: 10px; margin-bottom: 14px; }
  @media (max-width: 760px) { .cols { grid-template-columns: 1fr; } }
  .panel { background: var(--panel); border: 1px solid var(--border); border-radius: 8px; padding: 12px 14px; overflow-x: auto; }
  .panel h2 { font-size: 13px; color: var(--ink-3); text-transform: uppercase; letter-spacing: .04em; font-weight: 600; margin-bottom: 8px; }
  table { border-collapse: collapse; width: 100%; font-variant-numeric: tabular-nums; }
  th { text-align: left; color: var(--ink-3); font-size: 12px; font-weight: 500; padding: 3px 12px 5px 0; border-bottom: 1px solid var(--border); }
  td { padding: 5px 12px 5px 0; border-bottom: 1px solid var(--border); color: var(--ink-2); white-space: nowrap; }
  tr:last-child td { border-bottom: none; }
  td.id { color: var(--ink); font-weight: 600; }
  .flag { font-weight: 650; }
  .flag.on { color: var(--critical); }
  .flag.off { color: var(--ink-3); font-weight: 400; }
  .empty { color: var(--ink-3); padding: 8px 0; }
</style>
</head>
<body>
<header>
  <h1>rtlamr scan</h1>
  <span class="sub" id="meta">connecting&hellip;</span>
</header>

<div class="banner" id="gps"><span class="dot">&#9679;</span> waiting for data&hellip;</div>

<div class="tiles">
  <div class="tile"><div class="label">Meters found</div><div class="value" id="meters">0</div></div>
  <div class="tile"><div class="label">Leak flagged</div><div class="value" id="leaking">0</div></div>
  <div class="tile"><div class="label">Bursts captured</div><div class="value" id="bursts">0</div></div>
  <div class="tile"><div class="label">Runtime</div><div class="value" id="uptime">0:00</div></div>
</div>

<div class="cols">
  <div class="panel">
    <h2>Radios</h2>
    <table>
      <thead><tr><th>Radio</th><th>Center</th><th>Bursts</th><th>Last burst</th></tr></thead>
      <tbody id="radios"></tbody>
    </table>
  </div>
  <div class="panel">
    <h2>Leak-flagged meters</h2>
    <div id="leaktable"></div>
  </div>
</div>

<div class="panel">
  <h2>Recent bursts</h2>
  <div id="recent"></div>
</div>

<script>
"use strict";
const $ = id => document.getElementById(id);
const esc = s => String(s).replace(/[&<>"]/g, c => ({"&":"&amp;","<":"&lt;",">":"&gt;",'"':"&quot;"}[c]));

function fmtDur(sec) {
  sec = Math.max(0, sec|0);
  const h = (sec/3600)|0, m = ((sec%3600)/60)|0, s = sec%60;
  return h ? h + ":" + String(m).padStart(2,"0") + ":" + String(s).padStart(2,"0")
           : m + ":" + String(s).padStart(2,"0");
}
function fmtAgo(sec) {
  if (sec < 0) return "never";
  if (sec < 2) return "now";
  if (sec < 60) return sec + "s ago";
  return ((sec/60)|0) + "m ago";
}
function fmtMHz(hz) { return (hz/1e6).toFixed(2) + " MHz"; }
function fmtTime(t) {
  const m = String(t).match(/T(\d\d:\d\d:\d\d)/);
  return m ? m[1] : esc(t);
}

function renderGPS(d) {
  const el = $("gps");
  if (!d.gpsEnabled) {
    el.style.borderColor = "var(--border)";
    el.innerHTML = '<span class="dot" style="color:var(--ink-3)">&#9679;</span> GPS disabled ' +
      '<span class="detail">start with -gps COM13 to record positions</span>';
    return;
  }
  const g = d.gps;
  if (g.hasFix) {
    el.style.borderColor = "var(--good)";
    el.innerHTML = '<span class="dot" style="color:var(--good)">&#9679;</span> GPS FIX ' +
      '<span class="detail">' + g.lat.toFixed(6) + ", " + g.lon.toFixed(6) +
      " &middot; " + g.numSats + " sats &middot; HDOP " + g.hdop.toFixed(1) + "</span>";
  } else if (g.everHadFix) {
    el.style.borderColor = "var(--critical)";
    el.innerHTML = '<span class="dot" style="color:var(--critical)">&#10005;</span> GPS FIX LOST ' +
      '<span class="detail">rows are logging blank positions</span>';
  } else {
    el.style.borderColor = "var(--warning)";
    const talker = g.talker ? "puck talking ($" + esc(g.talker) + "GGA)" : "no NMEA seen yet";
    el.innerHTML = '<span class="dot" style="color:var(--warning)">&#9650;</span> WAITING FOR GPS FIX ' +
      '<span class="detail">' + talker + " &middot; needs sky view, cold start can take minutes</span>";
  }
}

function leakCell(v) {
  return v > 0 ? '<span class="flag on">' + v + "</span>" : '<span class="flag off">0</span>';
}

function render(d) {
  $("meta").textContent = d.mode + " mode";
  $("meters").textContent = d.uniqueMeters;
  $("leaking").textContent = d.leakingMeters;
  $("leaking").className = "value" + (d.leakingMeters > 0 ? " crit" : "");
  $("bursts").textContent = d.totalBursts;
  $("uptime").textContent = fmtDur(d.uptimeSec);
  renderGPS(d);

  $("radios").innerHTML = (d.radios || []).map(r =>
    "<tr><td class=id>" + r.device + "</td><td>" + fmtMHz(r.freqHz) + "</td><td>" +
    r.bursts + "</td><td>" + fmtAgo(r.lastBurstSec) + "</td></tr>").join("");

  if ((d.leaking || []).length === 0) {
    $("leaktable").innerHTML = '<div class="empty">none seen this run</div>';
  } else {
    $("leaktable").innerHTML =
      "<table><thead><tr><th>Meter ID</th><th>LeakNow</th><th>Leak</th><th>Bursts</th><th>RSSI</th><th>Seen</th></tr></thead><tbody>" +
      d.leaking.map(m =>
        "<tr><td class=id>" + esc(m.id) + "</td><td>" + leakCell(m.leakNow) + "</td><td>" +
        leakCell(m.leak) + "</td><td>" + m.bursts + "</td><td>" + esc(m.lastRssi || "") +
        "</td><td>" + fmtAgo(m.lastSeenSec) + "</td></tr>").join("") +
      "</tbody></table>";
  }

  if ((d.recent || []).length === 0) {
    $("recent").innerHTML = '<div class="empty">no bursts yet &mdash; leave it running, meters transmit intermittently</div>';
  } else {
    $("recent").innerHTML =
      "<table><thead><tr><th>Time</th><th>Radio</th><th>Meter ID</th><th>RSSI</th><th>LeakNow</th><th>Leak</th></tr></thead><tbody>" +
      d.recent.map(r =>
        "<tr><td>" + fmtTime(r.time) + "</td><td>" + r.radio + "</td><td class=id>" + esc(r.id) +
        "</td><td>" + esc(r.rssi || "") + "</td><td>" + leakCell(r.leakNow) + "</td><td>" +
        leakCell(r.leak) + "</td></tr>").join("") +
      "</tbody></table>";
  }
}

async function poll() {
  try {
    const resp = await fetch("/stats");
    render(await resp.json());
  } catch (e) {
    $("meta").textContent = "scan stopped or unreachable";
  }
}
poll();
setInterval(poll, 1000);
</script>
</body>
</html>
`
