package main

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/mdp/qrterminal/v3"
)

var downloadDir string

func init() {
	exe, err := os.Executable()
	if err != nil {
		downloadDir = "localdrop_downloads"
		return
	}
	downloadDir = filepath.Join(filepath.Dir(exe), "localdrop_downloads")
}

const uploadHTML = `<!DOCTYPE html>
<html lang="en">
<head>
<meta charset="UTF-8">
<meta name="viewport" content="width=device-width, initial-scale=1.0">
<title>LocalDrop</title>
<style>
* { box-sizing: border-box; margin: 0; padding: 0; }
body { font-family: -apple-system, system-ui, sans-serif; background: #f5f5f5; color: #333; padding: 20px; }
h1 { text-align: center; margin-bottom: 8px; }
p.sub { text-align: center; color: #666; margin-bottom: 24px; }
form { max-width: 400px; margin: 0 auto; }
label.file-box {
  display: block; padding: 40px 20px; border: 3px dashed #aaa;
  border-radius: 12px; text-align: center; cursor: pointer;
  background: #fff; margin-bottom: 16px; font-size: 18px; color: #555;
}
label.file-box.has-file { border-color: #4caf50; color: #333; }
input[type="file"] { position: absolute; left: -9999px; opacity: 0; }
button {
  display: block; width: 100%; padding: 16px; font-size: 18px;
  background: #4caf50; color: #fff; border: none; border-radius: 8px; cursor: pointer;
}
.file-box.drag { border-color: #4caf50; background: #e8f5e9; }
button:disabled { background: #aaa; }
#list { max-width: 400px; margin: 16px auto 0; }
.item { background: #fff; border-radius: 8px; padding: 10px 12px; margin-bottom: 8px; }
.item .name { font-size: 15px; overflow: hidden; text-overflow: ellipsis; white-space: nowrap; }
.item .bar-row { display: flex; align-items: center; gap: 8px; margin-top: 6px; }
.item progress { flex: 1; height: 12px; }
.item .x { background: none; color: #999; border: none; padding: 0 4px; font-size: 20px; line-height: 1; cursor: pointer; }
.item .x:disabled { visibility: hidden; }
.item .st-row { display: flex; justify-content: space-between; gap: 8px; font-size: 13px; color: #666; margin-top: 4px; }
.item .spd { white-space: nowrap; font-variant-numeric: tabular-nums; }
#status { text-align: center; margin-top: 16px; font-size: 16px; }
#speed { text-align: center; margin-top: 6px; font-size: 14px; color: #666; font-variant-numeric: tabular-nums; }
.ok { color: #4caf50; } .err { color: #e53935; }
</style>
</head>
<body>
<h1>LocalDrop</h1>
<p class="sub">Tap the box below to pick one or more files, then upload them.</p>
<form id="f">
  <label class="file-box" id="lbl" for="file">Tap here to choose files</label>
  <input type="file" id="file" name="file" accept="*/*" multiple>
  <button type="submit" id="btn" disabled>Upload</button>
</form>
<div id="list"></div>
<div id="status"></div>
<div id="speed" class="sub"></div>
<script>
const CHUNK = 32 * 1024 * 1024;
const MAX_RETRIES = 8;
const PARALLEL = 3; // files uploaded at the same time; chunks within a file stay sequential
const file = document.getElementById('file');
const lbl = document.getElementById('lbl');
const btn = document.getElementById('btn');
const list = document.getElementById('list');
const st = document.getElementById('status');
const speedEl = document.getElementById('speed');

// mbps formats a byte rate as megabits per second, like the server log.
const mbps = (bytes, secs) => (secs > 0 ? (bytes * 8 / secs / 1e6).toFixed(1) : '0.0') + ' Mbps';
const mb = (bytes) => (bytes / 1e6).toFixed(1) + ' MB';

let audio = null;

// chime plays a short two-note beep when a file finishes.
function chime() {
  if (!audio) return;
  const t = audio.currentTime;
  [880, 1320].forEach((freq, i) => {
    const osc = audio.createOscillator();
    const gain = audio.createGain();
    osc.frequency.value = freq;
    gain.gain.setValueAtTime(0.2, t + i * 0.15);
    gain.gain.exponentialRampToValueAtTime(0.001, t + i * 0.15 + 0.3);
    osc.connect(gain).connect(audio.destination);
    osc.start(t + i * 0.15);
    osc.stop(t + i * 0.15 + 0.3);
  });
}

const fileKey = (f) => f.name + '|' + f.size + '|' + f.lastModified;
const completed = new Set(); // files already saved, so a retry doesn't upload them again
let busy = false;

file.addEventListener('change', () => {
  if (file.files.length > 0) {
    const n = file.files.length;
    lbl.textContent = n === 1 ? file.files[0].name : n + ' files selected';
    lbl.classList.add('has-file');
    btn.disabled = busy;
  }
});

const cancelError = () => Object.assign(new Error('Cancelled'), { fatal: true, cancelled: true });

// api resolves with {status, data} for any HTTP response and rejects on network errors.
// ctl.cancel() aborts the request in flight.
function api(ctl, method, url, body, onProgress) {
  return new Promise((resolve, reject) => {
    if (ctl.cancelled) return reject(cancelError());
    const xhr = new XMLHttpRequest();
    ctl.abort = () => xhr.abort();
    xhr.onabort = () => reject(cancelError());
    xhr.open(method, url);
    if (onProgress) xhr.upload.onprogress = onProgress;
    xhr.onload = () => {
      let data = {};
      try { data = JSON.parse(xhr.responseText); } catch (e) {}
      resolve({ status: xhr.status, data: data, text: xhr.responseText });
    };
    xhr.onerror = () => reject(new Error('Network error'));
    xhr.send(body);
  });
}

// cancellable sleep: resolves early when ctl.cancel() is called.
function pause(ctl, ms) {
  return new Promise((resolve) => {
    const t = setTimeout(resolve, ms);
    ctl.abort = () => { clearTimeout(t); resolve(); };
  });
}

// upload sends f in chunks. Cancelling leaves the partial file on the server,
// so sending the same file again resumes where it stopped.
async function upload(f, bar, say, ctl, track) {
  const q = new URLSearchParams({ name: f.name, size: f.size, mtime: f.lastModified });
  let id = null, offset = 0, done = false, fails = 0;
  while (!done) {
    if (ctl.cancelled) throw cancelError();
    try {
      if (id === null) {
        const r = await api(ctl, 'POST', '/upload/init?' + q);
        // 423: a cancelled request for this file is still winding down on the server.
        if (r.status !== 200) throw Object.assign(new Error(r.text), { fatal: r.status < 500 && r.status !== 423 });
        id = r.data.id;
        offset = r.data.offset;
        done = r.data.done;
        if (offset > 0) say('Resuming from ' + Math.round(offset / 1e6) + ' MB...');
        bar.value = f.size ? (offset / f.size) * 100 : 0;
        continue;
      }
      const end = Math.min(offset + CHUNK, f.size);
      const base = offset;
      let sent = 0;
      const r = await api(ctl, 'PUT', '/upload/chunk?id=' + id + '&offset=' + offset, f.slice(offset, end), (ev) => {
        track(ev.loaded - sent);
        sent = ev.loaded;
        if (ev.lengthComputable) bar.value = ((base + ev.loaded) / f.size) * 100;
      });
      if (r.status === 200) {
        offset = r.data.offset;
        done = r.data.done;
        fails = 0;
        say('Uploading...');
      } else if (r.status === 409) {
        offset = r.data.offset; // server is the source of truth
      } else if (r.status === 404) {
        id = null; // server forgot this upload, re-init
      } else {
        throw Object.assign(new Error(r.text), { fatal: r.status < 500 && r.status !== 423 });
      }
    } catch (err) {
      if (err.fatal || ++fails > MAX_RETRIES) throw err;
      say('Connection problem, retrying (' + fails + '/' + MAX_RETRIES + ')...');
      await pause(ctl, Math.min(1000 * 2 ** (fails - 1), 10000));
      if (ctl.cancelled) throw cancelError();
      if (id !== null) {
        try {
          const r = await api(ctl, 'GET', '/upload/offset?id=' + id);
          if (r.status === 200) offset = r.data.offset;
        } catch (e) {}
      }
    }
  }
}

function addRow(f) {
  const row = document.createElement('div');
  row.className = 'item';
  row.innerHTML = '<div class="name"></div><div class="bar-row"><progress value="0" max="100"></progress>' +
    '<button type="button" class="x" title="Cancel upload">&times;</button></div>' +
    '<div class="st-row"><span class="st">Waiting...</span><span class="spd"></span></div>';
  row.querySelector('.name').textContent = f.name;
  list.appendChild(row);
  const stEl = row.querySelector('.st');
  const x = row.querySelector('.x');
  const spdEl = row.querySelector('.spd');
  const ctl = { cancelled: false, abort: () => {} };
  ctl.cancel = () => { ctl.cancelled = true; x.disabled = true; ctl.abort(); };
  x.addEventListener('click', ctl.cancel);
  return {
    bar: row.querySelector('progress'),
    say: (t, cls) => { stEl.textContent = t; stEl.className = 'st ' + (cls || ''); },
    ctl,
    finished: () => { x.disabled = true; },
    speed: (t) => { spdEl.textContent = t; },
    sent: 0,     // bytes sent this session
    lastSent: 0, // value of sent at the previous speed tick
    start: 0,    // when this file started sending
    active: false,
  };
}

// Without this the browser opens a dropped file instead of uploading it.
window.addEventListener('dragover', (e) => { e.preventDefault(); lbl.classList.add('drag'); });
window.addEventListener('dragleave', (e) => { if (!e.relatedTarget) lbl.classList.remove('drag'); });
window.addEventListener('drop', (e) => {
  e.preventDefault();
  lbl.classList.remove('drag');
  if (busy || !e.dataTransfer.files.length) return;
  file.files = e.dataTransfer.files;
  file.dispatchEvent(new Event('change'));
});

document.getElementById('f').addEventListener('submit', async (e) => {
  e.preventDefault();
  if (busy || !file.files.length) return;
  // Drop duplicates in the selection (they share an upload id on the server)
  // and files a previous attempt already saved.
  const seen = new Set();
  const files = Array.from(file.files).filter((f) => {
    const k = fileKey(f);
    return !completed.has(k) && !seen.has(k) && seen.add(k);
  });
  if (!files.length) {
    st.className = 'ok';
    st.textContent = 'These files were already uploaded.';
    return;
  }
  busy = true;
  // Browsers only allow audio after a user gesture, so create it on submit.
  if (!audio && window.AudioContext) audio = new AudioContext();
  list.textContent = '';
  btn.disabled = true;
  st.className = '';
  st.textContent = 'Uploading... do not leave or lock this page.';
  let wakeLock = null;
  if (navigator.wakeLock) {
    navigator.wakeLock.request('screen').then(l => wakeLock = l).catch(() => {});
  }
  const rows = files.map(addRow);
  const startedAt = performance.now();
  let totalSent = 0, lastTotal = 0;
  const TICK = 1000;
  const ticker = setInterval(() => {
    for (const row of rows) {
      if (!row.active) continue;
      row.speed(mbps(row.sent - row.lastSent, TICK / 1000));
      row.lastSent = row.sent;
    }
    speedEl.textContent = 'Total: ' + mb(totalSent) + ' at ' + mbps(totalSent - lastTotal, TICK / 1000);
    lastTotal = totalSent;
  }, TICK);
  const failed = [];
  let cancelled = 0;
  let next = 0;
  async function worker() {
    while (next < files.length) {
      const i = next++;
      const row = rows[i];
      const { bar, say, ctl, finished } = row;
      try {
        if (ctl.cancelled) throw cancelError(); // cancelled while waiting in the queue
        say('Uploading...');
        row.start = performance.now();
        row.active = true;
        await upload(files[i], bar, say, ctl, (n) => { row.sent += n; totalSent += n; });
        completed.add(fileKey(files[i]));
        bar.value = 100;
        say('Done', 'ok');
        chime();
      } catch (err) {
        if (err.cancelled) {
          say('Cancelled (send it again to resume)', 'err');
          cancelled++;
        } else {
          say('Failed: ' + err.message, 'err');
          failed.push(files[i].name);
        }
      }
      if (row.active) {
        row.active = false;
        row.speed(row.sent ? 'avg ' + mbps(row.sent, (performance.now() - row.start) / 1000) : '');
      }
      finished();
    }
  }
  await Promise.all(Array.from({ length: Math.min(PARALLEL, files.length) }, worker));
  clearInterval(ticker);
  speedEl.textContent = 'Total: ' + mb(totalSent) + ', avg ' + mbps(totalSent, (performance.now() - startedAt) / 1000);
  if (failed.length || cancelled) {
    const parts = [];
    if (failed.length) parts.push(failed.length + ' failed');
    if (cancelled) parts.push(cancelled + ' cancelled');
    st.className = 'err';
    st.textContent = parts.join(', ') + ' of ' + files.length + ' (select them again to resume)';
  } else {
    st.className = 'ok';
    st.textContent = files.length === 1 ? 'Upload complete!' : 'All ' + files.length + ' files uploaded!';
    file.value = '';
    lbl.textContent = 'Tap here to choose files';
    lbl.classList.remove('has-file');
  }
  if (wakeLock) wakeLock.release();
  busy = false;
  btn.disabled = !file.files.length;
});
</script>
</body>
</html>`

func getLocalIP() string {
	ifaces, err := net.Interfaces()
	if err != nil {
		return "127.0.0.1"
	}
	for _, iface := range ifaces {
		if iface.Flags&net.FlagUp == 0 || iface.Flags&net.FlagLoopback != 0 {
			continue
		}
		addrs, err := iface.Addrs()
		if err != nil {
			continue
		}
		for _, addr := range addrs {
			var ip net.IP
			switch v := addr.(type) {
			case *net.IPNet:
				ip = v.IP
			case *net.IPAddr:
				ip = v.IP
			}
			if ip == nil || ip.IsLoopback() {
				continue
			}
			ip = ip.To4()
			if ip != nil {
				return ip.String()
			}
		}
	}
	return "127.0.0.1"
}

// uniquePath returns a path that doesn't already exist, appending (1), (2), etc. if needed.
func uniquePath(dir, filename string) string {
	dest := filepath.Join(dir, filename)
	if _, err := os.Stat(dest); os.IsNotExist(err) {
		return dest
	}
	ext := filepath.Ext(filename)
	name := strings.TrimSuffix(filename, ext)
	for i := 1; ; i++ {
		dest = filepath.Join(dir, fmt.Sprintf("%s(%d)%s", name, i, ext))
		if _, err := os.Stat(dest); os.IsNotExist(err) {
			return dest
		}
	}
}

// received counts bytes written by all chunk uploads; logReceived reports the
// aggregate rate, since a single chunk usually finishes before one tick.
var received atomic.Int64

// countingWriter counts bytes both globally and for its upload.
type countingWriter struct {
	dst io.Writer
	u   *upload
}

func (cw countingWriter) Write(p []byte) (int, error) {
	n, err := cw.dst.Write(p)
	received.Add(int64(n))
	cw.u.received.Add(int64(n))
	return n, err
}

func mbps(bytes int64, d time.Duration) float64 {
	if d <= 0 {
		return 0
	}
	return float64(bytes) * 8 / d.Seconds() / 1_000_000
}

func logReceived() {
	const interval = 5 * time.Second
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	var last int64
	for range ticker.C {
		current := received.Load()
		delta := current - last
		last = current
		if delta == 0 {
			continue
		}
		fmt.Printf("  Total: %.1f MB received, %.1f Mbps\n", float64(current)/1_000_000, mbps(delta, interval))

		uploadsMu.Lock()
		for _, u := range uploads {
			cur := u.received.Load()
			d := cur - u.lastLogged
			u.lastLogged = cur
			if d == 0 {
				continue
			}
			fmt.Printf("    %s: %.1f Mbps\n", u.name, mbps(d, interval))
		}
		uploadsMu.Unlock()
	}
}

const chunkSize = 32 << 20

type upload struct {
	id   string
	name string
	size int64
	mu   sync.Mutex // held while a chunk is being written

	received   atomic.Int64 // bytes received since this server started
	started    atomic.Int64 // unix nanos of the first chunk, 0 until then
	lastLogged int64        // received at the previous log tick; guarded by uploadsMu
}

var (
	uploadsMu sync.Mutex
	uploads   = map[string]*upload{}
	finishMu  sync.Mutex // serializes picking a free name and renaming into it
)

func (u *upload) partPath() string {
	return filepath.Join(downloadDir, u.id+".part")
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	json.NewEncoder(w).Encode(v)
}

func getUpload(id string) *upload {
	uploadsMu.Lock()
	defer uploadsMu.Unlock()
	return uploads[id]
}

// finish moves the completed temp file to its final, non-colliding name.
func (u *upload) finish() error {
	finishMu.Lock()
	finalPath := uniquePath(downloadDir, u.name)
	err := os.Rename(u.partPath(), finalPath)
	finishMu.Unlock()
	if err != nil {
		return err
	}
	uploadsMu.Lock()
	delete(uploads, u.id)
	uploadsMu.Unlock()
	if start := u.started.Load(); start != 0 {
		fmt.Printf("\aSaved: %s (%.1f MB, avg %.1f Mbps)\n", finalPath, float64(u.size)/1_000_000,
			mbps(u.received.Load(), time.Since(time.Unix(0, start))))
	} else {
		fmt.Printf("\aSaved: %s (%.1f MB)\n", finalPath, float64(u.size)/1_000_000)
	}
	return nil
}

// handleInit starts an upload, or resumes it if the same file (name, size, mtime)
// was already partially received.
func handleInit(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	name := filepath.Base(q.Get("name"))
	size, err := strconv.ParseInt(q.Get("size"), 10, 64)
	if name == "." || name == string(filepath.Separator) || err != nil || size < 0 {
		http.Error(w, "invalid name or size", http.StatusBadRequest)
		return
	}
	sum := sha256.Sum256([]byte(fmt.Sprintf("%s|%d|%s", name, size, q.Get("mtime"))))
	id := hex.EncodeToString(sum[:16])

	if err := os.MkdirAll(downloadDir, 0755); err != nil {
		http.Error(w, "server error", http.StatusInternalServerError)
		return
	}

	uploadsMu.Lock()
	u := uploads[id]
	if u == nil {
		u = &upload{id: id, name: name, size: size}
		uploads[id] = u
	}
	uploadsMu.Unlock()

	if !u.mu.TryLock() {
		http.Error(w, "upload in progress", http.StatusLocked)
		return
	}
	defer u.mu.Unlock()

	f, err := os.OpenFile(u.partPath(), os.O_CREATE|os.O_WRONLY, 0644)
	if err != nil {
		http.Error(w, "server error", http.StatusInternalServerError)
		return
	}
	fi, err := f.Stat()
	f.Close()
	if err != nil {
		http.Error(w, "server error", http.StatusInternalServerError)
		return
	}
	offset := fi.Size()
	if offset > size { // stale part file from a different upload; start over
		offset = 0
		if err := os.Truncate(u.partPath(), 0); err != nil {
			http.Error(w, "server error", http.StatusInternalServerError)
			return
		}
	}
	done := offset == size
	if done {
		if err := u.finish(); err != nil {
			http.Error(w, "error finalizing file", http.StatusInternalServerError)
			return
		}
	}
	writeJSON(w, http.StatusOK, map[string]any{"id": id, "offset": offset, "done": done})
}

func handleOffset(w http.ResponseWriter, r *http.Request) {
	u := getUpload(r.URL.Query().Get("id"))
	if u == nil {
		http.Error(w, "unknown upload", http.StatusNotFound)
		return
	}
	fi, err := os.Stat(u.partPath())
	if err != nil {
		http.Error(w, "unknown upload", http.StatusNotFound)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"offset": fi.Size()})
}

// handleChunk appends the request body to the temp file. The offset must equal the
// bytes already on disk, so chunks can never overlap or leave gaps.
func handleChunk(w http.ResponseWriter, r *http.Request) {
	u := getUpload(r.URL.Query().Get("id"))
	if u == nil {
		http.Error(w, "unknown upload", http.StatusNotFound)
		return
	}
	offset, err := strconv.ParseInt(r.URL.Query().Get("offset"), 10, 64)
	if err != nil {
		http.Error(w, "invalid offset", http.StatusBadRequest)
		return
	}
	if !u.mu.TryLock() { // a previous request for this upload is still winding down
		http.Error(w, "upload in progress", http.StatusLocked)
		return
	}
	defer u.mu.Unlock()

	f, err := os.OpenFile(u.partPath(), os.O_WRONLY|os.O_APPEND, 0644)
	if err != nil {
		http.Error(w, "unknown upload", http.StatusNotFound)
		return
	}
	defer f.Close()
	fi, err := f.Stat()
	if err != nil {
		http.Error(w, "server error", http.StatusInternalServerError)
		return
	}
	cur := fi.Size()
	if offset != cur {
		writeJSON(w, http.StatusConflict, map[string]any{"offset": cur})
		return
	}
	if r.ContentLength < 0 || r.ContentLength > chunkSize || cur+r.ContentLength > u.size {
		http.Error(w, "invalid chunk length", http.StatusBadRequest)
		return
	}

	u.started.CompareAndSwap(0, time.Now().UnixNano())
	n, err := io.Copy(countingWriter{f, u}, http.MaxBytesReader(w, r.Body, r.ContentLength))
	if err != nil || n != r.ContentLength {
		// Whatever reached the disk stays; the client re-queries the offset and continues.
		http.Error(w, "error saving chunk", http.StatusInternalServerError)
		return
	}

	done := cur+n == u.size
	if done {
		f.Close()
		if err := u.finish(); err != nil {
			http.Error(w, "error finalizing file", http.StatusInternalServerError)
			return
		}
	}
	writeJSON(w, http.StatusOK, map[string]any{"offset": cur + n, "done": done})
}

func main() {
	listener, err := net.Listen("tcp", ":0")
	if err != nil {
		fmt.Fprintf(os.Stderr, "Failed to listen: %v\n", err)
		os.Exit(1)
	}

	port := listener.Addr().(*net.TCPAddr).Port
	ip := getLocalIP()
	url := fmt.Sprintf("http://%s:%d", ip, port)

	fmt.Printf("\nLocalDrop running at: %s\n\n", url)

	cfg := qrterminal.Config{
		Level:     qrterminal.L,
		Writer:    os.Stdout,
		BlackChar: "██",
		WhiteChar: "  ",
		QuietZone: 4,
	}
	qrterminal.GenerateWithConfig(url, cfg)

	fmt.Println("\nScan the QR code with your phone to upload files.")
	fmt.Printf("Can't scan the QR code? Open this address in your phone's browser instead:\n  %s\n", url)
	fmt.Println("Press Ctrl+C to stop.\n")

	http.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/" {
			http.NotFound(w, r)
			return
		}
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		fmt.Fprint(w, uploadHTML)
	})
	go logReceived()
	http.HandleFunc("POST /upload/init", handleInit)
	http.HandleFunc("GET /upload/offset", handleOffset)
	http.HandleFunc("PUT /upload/chunk", handleChunk)

	if err := http.Serve(listener, nil); err != nil {
		fmt.Fprintf(os.Stderr, "Server error: %v\n", err)
		os.Exit(1)
	}
}
