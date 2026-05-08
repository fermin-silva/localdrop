package main

import (
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"time"

	"github.com/mdp/qrterminal/v3"
)

const downloadDir = "localdrop_downloads"

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
#progress-wrap { display: none; margin-top: 16px; }
#progress-bar { width: 100%; height: 24px; border-radius: 8px; }
#status { text-align: center; margin-top: 16px; font-size: 16px; }
.ok { color: #4caf50; } .err { color: #e53935; }
</style>
</head>
<body>
<h1>LocalDrop</h1>
<p class="sub">Tap the box below to pick a file, then upload it.</p>
<form id="f">
  <label class="file-box" id="lbl" for="file">Tap here to choose a file</label>
  <input type="file" id="file" name="file" accept="*/*">
  <button type="submit" id="btn" disabled>Upload</button>
</form>
<div id="progress-wrap"><progress id="progress-bar" value="0" max="100"></progress></div>
<div id="status"></div>
<script>
const file = document.getElementById('file');
const lbl = document.getElementById('lbl');
const btn = document.getElementById('btn');
const bar = document.getElementById('progress-bar');
const pw = document.getElementById('progress-wrap');
const st = document.getElementById('status');

file.addEventListener('change', () => {
  if (file.files.length > 0) {
    lbl.textContent = file.files[0].name;
    lbl.classList.add('has-file');
    btn.disabled = false;
  }
});

document.getElementById('f').addEventListener('submit', (e) => {
  e.preventDefault();
  if (!file.files.length) return;
  const fd = new FormData();
  fd.append('file', file.files[0]);
  const xhr = new XMLHttpRequest();
  xhr.open('POST', '/upload');
  pw.style.display = 'block';
  btn.disabled = true;
  st.className = '';
  st.textContent = 'Uploading... do not leave or lock this page.';
  let wakeLock = null;
  if (navigator.wakeLock) {
    navigator.wakeLock.request('screen').then(l => wakeLock = l).catch(() => {});
  }
  xhr.upload.onprogress = (ev) => {
    if (ev.lengthComputable) bar.value = (ev.loaded / ev.total) * 100;
  };
  const releaseWake = () => { if (wakeLock) { wakeLock.release(); wakeLock = null; } };
  xhr.onload = () => {
    releaseWake();
    if (xhr.status === 200) {
      st.className = 'ok';
      st.textContent = 'Upload complete!';
      file.value = '';
      lbl.textContent = 'Tap here to choose a file';
      lbl.classList.remove('has-file');
    } else {
      st.className = 'err';
      st.textContent = 'Upload failed: ' + xhr.responseText;
    }
    pw.style.display = 'none';
    bar.value = 0;
    btn.disabled = false;
  };
  xhr.onerror = () => {
    releaseWake();
    st.className = 'err';
    st.textContent = 'Network error';
    pw.style.display = 'none';
    btn.disabled = false;
  };
  xhr.send(fd);
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

func handleUpload(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}

	mr, err := r.MultipartReader()
	if err != nil {
		http.Error(w, "invalid multipart request", http.StatusBadRequest)
		return
	}

	if err := os.MkdirAll(downloadDir, 0755); err != nil {
		http.Error(w, "server error", http.StatusInternalServerError)
		return
	}

	for {
		part, err := mr.NextPart()
		if err == io.EOF {
			break
		}
		if err != nil {
			http.Error(w, "error reading upload", http.StatusBadRequest)
			return
		}

		filename := part.FileName()
		if filename == "" {
			continue
		}
		// Sanitize: use only the base name
		filename = filepath.Base(filename)

		pendingPath := filepath.Join(downloadDir, "pending_"+filename)
		f, err := os.Create(pendingPath)
		if err != nil {
			http.Error(w, "server error", http.StatusInternalServerError)
			return
		}

		pw := newProgressWriter(f, filename)
		start := time.Now()
		n, err := io.Copy(pw, part)
		pw.stop()
		f.Close()
		if err != nil {
			os.Remove(pendingPath)
			http.Error(w, "error saving file", http.StatusInternalServerError)
			return
		}

		elapsed := time.Since(start).Seconds()
		avgMbps := 0.0
		if elapsed > 0 {
			avgMbps = float64(n) * 8 / elapsed / 1_000_000
		}

		finalPath := uniquePath(downloadDir, filename)
		if err := os.Rename(pendingPath, finalPath); err != nil {
			http.Error(w, "error finalizing file", http.StatusInternalServerError)
			return
		}

		fmt.Printf("Saved: %s (%.1f MB, %.1f Mbps avg, %.1fs)\n", finalPath, float64(n)/1_000_000, avgMbps, elapsed)
	}

	w.WriteHeader(http.StatusOK)
	fmt.Fprint(w, "OK")
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
	http.HandleFunc("/upload", handleUpload)

	if err := http.Serve(listener, nil); err != nil {
		fmt.Fprintf(os.Stderr, "Server error: %v\n", err)
		os.Exit(1)
	}
}
