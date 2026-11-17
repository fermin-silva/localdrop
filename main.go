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
button:disabled { background: #aaa; }
#list { max-width: 400px; margin: 16px auto 0; }
.item { background: #fff; border-radius: 8px; padding: 10px 12px; margin-bottom: 8px; }
.item .name { font-size: 15px; overflow: hidden; text-overflow: ellipsis; white-space: nowrap; }
.item progress { width: 100%; height: 12px; margin-top: 6px; }
.item .st { font-size: 13px; color: #666; margin-top: 4px; }
#status { text-align: center; margin-top: 16px; font-size: 16px; }
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
<script>
const CHUNK = 32 * 1024 * 1024;
const MAX_RETRIES = 8;
const PARALLEL = 3; // files uploaded at the same time; chunks within a file stay sequential
const file = document.getElementById('file');
const lbl = document.getElementById('lbl');
const btn = document.getElementById('btn');
const list = document.getElementById('list');
const st = document.getElementById('status');

const sleep = (ms) => new Promise((r) => setTimeout(r, ms));
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

// api resolves with {status, data} for any HTTP response and rejects on network errors.
function api(method, url, body, onProgress) {
  return new Promise((resolve, reject) => {
    const xhr = new XMLHttpRequest();
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

async function upload(f, bar, say) {
  const q = new URLSearchParams({ name: f.name, size: f.size, mtime: f.lastModified });
  let id = null, offset = 0, done = false, fails = 0;
  while (!done) {
    try {
      if (id === null) {
        const r = await api('POST', '/upload/init?' + q);
        if (r.status !== 200) throw Object.assign(new Error(r.text), { fatal: r.status < 500 });
        id = r.data.id;
        offset = r.data.offset;
        done = r.data.done;
        if (offset > 0) say('Resuming from ' + Math.round(offset / 1e6) + ' MB...');
        bar.value = f.size ? (offset / f.size) * 100 : 0;
        continue;
      }
      const end = Math.min(offset + CHUNK, f.size);
      const base = offset;
      const r = await api('PUT', '/upload/chunk?id=' + id + '&offset=' + offset, f.slice(offset, end), (ev) => {
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
      await sleep(Math.min(1000 * 2 ** (fails - 1), 10000));
      if (id !== null) {
        try {
          const r = await api('GET', '/upload/offset?id=' + id);
          if (r.status === 200) offset = r.data.offset;
        } catch (e) {}
      }
    }
  }
}

function addRow(f) {
  const row = document.createElement('div');
  row.className = 'item';
  row.innerHTML = '<div class="name"></div><progress value="0" max="100"></progress><div class="st">Waiting...</div>';
  row.querySelector('.name').textContent = f.name;
  list.appendChild(row);
  const stEl = row.querySelector('.st');
  return {
    bar: row.querySelector('progress'),
    say: (t, cls) => { stEl.textContent = t; stEl.className = 'st ' + (cls || ''); },
  };
}

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
  list.textContent = '';
  btn.disabled = true;
  st.className = '';
  st.textContent = 'Uploading... do not leave or lock this page.';
  let wakeLock = null;
  if (navigator.wakeLock) {
    navigator.wakeLock.request('screen').then(l => wakeLock = l).catch(() => {});
  }
  const rows = files.map(addRow);
  const failed = [];
  let next = 0;
  async function worker() {
    while (next < files.length) {
      const i = next++;
      const { bar, say } = rows[i];
      say('Uploading...');
      try {
        await upload(files[i], bar, say);
        completed.add(fileKey(files[i]));
        bar.value = 100;
        say('Done', 'ok');
      } catch (err) {
        say('Failed: ' + err.message, 'err');
        failed.push(files[i].name);
      }
    }
  }
  await Promise.all(Array.from({ length: Math.min(PARALLEL, files.length) }, worker));
  if (failed.length) {
    st.className = 'err';
    st.textContent = failed.length + ' of ' + files.length + ' failed (select them again to resume)';
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

type progressWriter struct {
	dst      io.Writer
	bytes    atomic.Int64
	done     chan struct{}
	filename string
}

func newProgressWriter(dst io.Writer, filename string) *progressWriter {
	pw := &progressWriter{dst: dst, filename: filename, done: make(chan struct{})}
	go pw.logLoop()
	return pw
}

func (pw *progressWriter) Write(p []byte) (int, error) {
	n, err := pw.dst.Write(p)
	pw.bytes.Add(int64(n))
	return n, err
}

func (pw *progressWriter) logLoop() {
	ticker := time.NewTicker(5 * time.Second)
	defer ticker.Stop()
	var last int64
	for {
		select {
		case <-ticker.C:
			current := pw.bytes.Load()
			delta := current - last
			last = current
			mbps := float64(delta) * 8 / 5 / 1_000_000
			fmt.Printf("  %s: %.1f MB received, %.1f Mbps\n", pw.filename, float64(current)/1_000_000, mbps)
		case <-pw.done:
			return
		}
	}
}

func (pw *progressWriter) stop() {
	close(pw.done)
}

const chunkSize = 32 << 20

type upload struct {
	id   string
	name string
	size int64
	mu   sync.Mutex // held while a chunk is being written
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
	fmt.Printf("Saved: %s (%.1f MB)\n", finalPath, float64(u.size)/1_000_000)
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

	pw := newProgressWriter(f, u.name)
	n, err := io.Copy(pw, http.MaxBytesReader(w, r.Body, r.ContentLength))
	pw.stop()
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
	fmt.Println("Press Ctrl+C to stop.\n")

	http.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/" {
			http.NotFound(w, r)
			return
		}
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		fmt.Fprint(w, uploadHTML)
	})
	http.HandleFunc("POST /upload/init", handleInit)
	http.HandleFunc("GET /upload/offset", handleOffset)
	http.HandleFunc("PUT /upload/chunk", handleChunk)

	if err := http.Serve(listener, nil); err != nil {
		fmt.Fprintf(os.Stderr, "Server error: %v\n", err)
		os.Exit(1)
	}
}
