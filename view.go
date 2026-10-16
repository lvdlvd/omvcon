// SPDX-License-Identifier: MIT
//
// Copyright (c) 2026 lvd
//
// omvcon view: a script's frames in a browser page, and the page's clicks
// and boxes back to the script.
//
// Theory of operation. view runs the script through the raw REPL as run
// does, after one exec that installs omvview.py (embedded in this binary)
// as the module omvview with ACTIVE set; see that file for the board side.
// The script's output is then one stream holding text and frame packets:
//
//	ff 4f 4d 56 46   magic, b"\xffOMVF"
//	u32 le           JPEG length n
//	u32 le           crc32 (IEEE, as zlib and binascii) of the JPEG
//	n bytes          the JPEG
//
// Text never holds 0xFF, so the demultiplexer below copies bytes to the
// terminal until it sees 0xFF (a packet, or a stray byte that is printed
// and skipped) or 0x04, which ends the script's output in the raw REPL.
// A packet is consumed by its length, so the 0x04 bytes inside JPEGs are
// never taken for that end. Packets with a bad crc are counted and dropped.
//
// The HTTP side serves the newest frame: /stream as MJPEG
// (multipart/x-mixed-replace), /frame.jpg for one, /status as JSON, and
// the page itself. The page posts "click X Y" or "box X Y W H" in JPEG
// pixels to /event; the handler checks and reformats the line, so only
// digits, letters, spaces and a newline reach the board (0x03 or 0x04
// there would interrupt the script or end the raw REPL input). The main
// loop writes queued events between reads, so the port has one user.

package main

import (
	"bytes"
	_ "embed"
	"encoding/binary"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"hash/crc32"
	"image/jpeg"
	"io"
	"net"
	"net/http"
	"os"
	"os/exec"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"time"
)

//go:embed omvview.py
var omvviewSource string

// injectCode makes omvview importable without a copy on the card. A module
// object cannot be made from Python on this MicroPython (the module type
// has no constructor), so the source runs in a dict and a class carries its
// names; import finds whatever sys.modules holds. The functions keep the
// dict as their globals, which is why setting omvview.X from the script
// changes nothing inside them.
const injectCode = `import sys
_d = {}
exec(%s, _d)
_d['ACTIVE'] = True
class omvview:
    pass
for _k in _d:
    if not _k.startswith('__'):
        setattr(omvview, _k, _d[_k])
sys.modules['omvview'] = omvview
del _d, _k, omvview
`

var (
	frameMagic = []byte("\xffOMVF")
	maxFrame   = 4 << 20 // a length beyond this is a corrupt header, not a frame
)

func viewCmd(path string, intr <-chan os.Signal, args []string) {
	fs := flag.NewFlagSet("view", flag.ExitOnError)
	addr := fs.String("http", "localhost:8080", "where to serve the page (\":8080\" for every interface)")
	open := fs.Bool("open", false, "open the page in the browser")
	fs.Parse(args)
	if fs.NArg() != 1 {
		flag.Usage()
		os.Exit(exitUsage)
	}
	code, err := os.ReadFile(fs.Arg(0))
	if err != nil {
		fail(exitUsage, err)
	}
	ln, err := net.Listen("tcp", *addr)
	if err != nil {
		fail(exitUsage, err)
	}

	b := attach(path, intr)
	exc, err := b.exec(fmt.Sprintf(injectCode, strconv.Quote(omvviewSource)), os.Stdout, 10*time.Second)
	if err != nil {
		fail(exitUnreachable, err)
	}
	if exc != "" {
		fmt.Fprint(os.Stderr, exc)
		fail(exitPython, errors.New("could not install omvview on the board"))
	}

	h := &hub{next: make(chan struct{}), events: make(chan string, 32)}
	go http.Serve(ln, h.mux())
	url := "http://" + *addr + "/"
	if strings.HasPrefix(*addr, ":") {
		url = "http://localhost" + *addr + "/"
	}
	fmt.Fprintf(os.Stderr, "omvcon: view at %s\n", url)
	if *open {
		opener := "xdg-open"
		if runtime.GOOS == "darwin" {
			opener = "open"
		}
		exec.Command(opener, url).Start()
	}

	if err := b.start(string(code)); err != nil {
		fail(exitUnreachable, err)
	}
	exc, err = h.demux(b, *fTimeout)
	h.summary()
	if err != nil {
		fail(exitUnreachable, err)
	}
	b.leaveRaw()
	if exc != "" {
		fmt.Fprint(os.Stderr, exc)
		exit(exitPython)
	}
}

// demux reads the running script's output until the raw REPL's end of
// output, then hands the exception and prompt to finish. On Ctrl-C or the
// timeout it sends Ctrl-C, and again every second: omvview switches the
// interrupt off while it writes a packet, and a script that does not call
// omvview.poll() never sees the one that landed in stdin then.
func (h *hub) demux(b *board, timeout time.Duration) (exc string, err error) {
	start := time.Now()
	var stopAt, lastIntr time.Time
	for {
		for len(b.in) > 0 {
			i := 0
			for i < len(b.in) && b.in[i] != 0xff && b.in[i] != 0x04 {
				i++
			}
			os.Stdout.Write(b.in[:i])
			b.in = b.in[i:]
			if len(b.in) == 0 {
				break
			}
			if b.in[0] == 0x04 {
				return b.finish(os.Stdout, 10*time.Second)
			}
			k := min(len(b.in), len(frameMagic))
			if !bytes.Equal(b.in[:k], frameMagic[:k]) {
				os.Stdout.Write(b.in[:1])
				b.in = b.in[1:]
				continue
			}
			if len(b.in) < len(frameMagic)+8 {
				break
			}
			n := int(binary.LittleEndian.Uint32(b.in[5:]))
			sum := binary.LittleEndian.Uint32(b.in[9:])
			if n > maxFrame {
				fmt.Fprintf(os.Stderr, "omvcon: frame header with length %d, skipped\n", n)
				b.in = b.in[1:]
				continue
			}
			if len(b.in) < 13+n {
				break
			}
			j := bytes.Clone(b.in[13 : 13+n])
			b.in = b.in[13+n:]
			h.publish(j, crc32.ChecksumIEEE(j) == sum)
		}

		if stopAt.IsZero() {
			select {
			case <-b.intr:
				stopAt = time.Now()
			default:
				if timeout > 0 && time.Since(start) > timeout {
					fmt.Fprintf(os.Stderr, "omvcon: %v passed, stopping the board's code\n", timeout)
					stopAt = time.Now()
				}
			}
		}
		if !stopAt.IsZero() {
			if time.Since(stopAt) > 10*time.Second {
				return "", errors.New("the board did not stop within 10 s of a Ctrl-C")
			}
			if time.Since(lastIntr) > time.Second {
				b.p.send("\x03")
				lastIntr = time.Now()
			}
		}

		// Events stop at the Ctrl-C: what the script does not read before
		// it ends would be read as code by the raw REPL.
		for drained := false; !drained && stopAt.IsZero(); {
			select {
			case ev := <-h.events:
				if err := b.p.write([]byte(ev+"\n"), 500*time.Millisecond); err != nil {
					fmt.Fprintf(os.Stderr, "omvcon: event %q not sent, the script is not reading them: %v\n", ev, err)
				}
			default:
				drained = true
			}
		}

		if _, err := b.fill(); err != nil {
			return "", err
		}
	}
}

// hub holds the newest frame for the HTTP side, and the counters.
type hub struct {
	mu     sync.Mutex
	jpeg   []byte
	next   chan struct{} // closed when a new frame arrives
	w, h   int
	frames int
	bad    int
	bytes  int
	first  time.Time
	recent []time.Time // arrivals in the last 2 s, for the fps
	event  string
	events chan string // to the board, one line each
}

func (h *hub) publish(j []byte, ok bool) {
	var w, ht int
	if ok {
		cfg, err := jpeg.DecodeConfig(bytes.NewReader(j))
		ok = err == nil
		w, ht = cfg.Width, cfg.Height
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	if !ok {
		h.bad++
		return
	}
	now := time.Now()
	if h.frames == 0 {
		h.first = now
	}
	h.frames++
	h.bytes += len(j)
	h.recent = append(h.recent, now)
	for len(h.recent) > 0 && now.Sub(h.recent[0]) > 2*time.Second {
		h.recent = h.recent[1:]
	}
	h.jpeg, h.w, h.h = j, w, ht
	close(h.next)
	h.next = make(chan struct{})
}

func (h *hub) summary() {
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.frames < 2 {
		fmt.Fprintf(os.Stderr, "omvcon: %d frames, %d bad\n", h.frames, h.bad)
		return
	}
	d := h.recent[len(h.recent)-1].Sub(h.first).Seconds()
	fmt.Fprintf(os.Stderr, "omvcon: %d frames (%dx%d) in %.1f s: %.1f fps, %.0f KB/s, %d bad\n",
		h.frames, h.w, h.h, d, float64(h.frames-1)/d, float64(h.bytes)/1024/d, h.bad)
}

func (h *hub) mux() *http.ServeMux {
	m := http.NewServeMux()
	m.HandleFunc("GET /{$}", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		w.Write([]byte(viewPage))
	})
	m.HandleFunc("GET /frame.jpg", func(w http.ResponseWriter, r *http.Request) {
		h.mu.Lock()
		j := h.jpeg
		h.mu.Unlock()
		if j == nil {
			http.Error(w, "no frame yet", http.StatusServiceUnavailable)
			return
		}
		w.Header().Set("Content-Type", "image/jpeg")
		w.Header().Set("Cache-Control", "no-store")
		w.Write(j)
	})
	m.HandleFunc("GET /stream", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "multipart/x-mixed-replace; boundary=omvframe")
		w.Header().Set("Cache-Control", "no-store")
		rc := http.NewResponseController(w)
		for {
			h.mu.Lock()
			j, next := h.jpeg, h.next
			h.mu.Unlock()
			if j != nil {
				fmt.Fprintf(w, "--omvframe\r\nContent-Type: image/jpeg\r\nContent-Length: %d\r\n\r\n", len(j))
				w.Write(j)
				w.Write([]byte("\r\n"))
				if rc.Flush() != nil {
					return
				}
			}
			select {
			case <-next:
			case <-r.Context().Done():
				return
			}
		}
	})
	m.HandleFunc("GET /status", func(w http.ResponseWriter, r *http.Request) {
		h.mu.Lock()
		fps := 0.0
		if n := len(h.recent); n > 1 && time.Since(h.recent[n-1]) < time.Second {
			fps = float64(n-1) / h.recent[n-1].Sub(h.recent[0]).Seconds()
		}
		s := map[string]any{"fps": fps, "frames": h.frames, "bad": h.bad, "w": h.w, "h": h.h, "event": h.event}
		h.mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(s)
	})
	m.HandleFunc("POST /event", func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(io.LimitReader(r.Body, 128))
		f := strings.Fields(string(body))
		v := make([]int, 0, 4)
		for _, s := range f[min(1, len(f)):] {
			x, err := strconv.Atoi(s)
			if err != nil || x < 0 || x > 65535 {
				break
			}
			v = append(v, x)
		}
		var ev string
		switch {
		case len(f) == 3 && f[0] == "click" && len(v) == 2:
			ev = fmt.Sprintf("click %d %d", v[0], v[1])
		case len(f) == 5 && f[0] == "box" && len(v) == 4:
			ev = fmt.Sprintf("box %d %d %d %d", v[0], v[1], v[2], v[3])
		default:
			http.Error(w, `want "click X Y" or "box X Y W H"`, http.StatusBadRequest)
			return
		}
		select {
		case h.events <- ev:
		default:
			http.Error(w, "event queue full", http.StatusServiceUnavailable)
			return
		}
		h.mu.Lock()
		h.event = ev
		h.mu.Unlock()
		w.WriteHeader(http.StatusNoContent)
	})
	return m
}

// The overlay maps the pointer from displayed to JPEG pixels; a drag of
// under 5 screen pixels counts as a click.
const viewPage = `<!doctype html>
<html><head><meta charset="utf-8"><title>omvcon view</title>
<meta name="viewport" content="width=device-width, initial-scale=1">
<style>
:root { --bg: #f4f4f2; --fg: #222; --dim: #666; --mark: #00a0ff; }
@media (prefers-color-scheme: dark) { :root { --bg: #151515; --fg: #ddd; --dim: #888; } }
body { margin: 0; padding: 12px 16px; background: var(--bg); color: var(--fg);
  font: 13px ui-monospace, Menlo, monospace; }
#wrap { position: relative; display: inline-block; max-width: 100%; }
#v { display: block; width: min(calc(100vw - 32px), 1024px); background: #000; image-rendering: pixelated; }
#c { position: absolute; inset: 0; width: 100%; height: 100%; cursor: crosshair; touch-action: none; }
#s { margin-top: 8px; white-space: pre-wrap; }
#hint { color: var(--dim); }
</style></head><body>
<div id="wrap"><img id="v" src="/stream" alt=""><canvas id="c"></canvas></div>
<div id="s">waiting for the first frame</div>
<div id="hint">click: spot reading there, drag: a box</div>
<script>
const v = document.getElementById('v'), c = document.getElementById('c'),
      s = document.getElementById('s'), g = c.getContext('2d');
let size = [0, 0], down = null, mark = null;

function dims() { return [v.naturalWidth || size[0], v.naturalHeight || size[1]]; }
function toImg(e) {
  const r = c.getBoundingClientRect(), [w, h] = dims();
  const x = Math.floor((e.clientX - r.left) * w / r.width), y = Math.floor((e.clientY - r.top) * h / r.height);
  return [Math.max(0, Math.min(w - 1, x)), Math.max(0, Math.min(h - 1, y)), e.clientX, e.clientY];
}
function draw() {
  const r = c.getBoundingClientRect(), d = window.devicePixelRatio || 1, [w, h] = dims();
  c.width = r.width * d; c.height = r.height * d;
  g.clearRect(0, 0, c.width, c.height);
  if (!mark || !w) return;
  const k = c.width / w, q = c.height / h;
  g.strokeStyle = getComputedStyle(document.documentElement).getPropertyValue('--mark');
  g.lineWidth = 2 * d;
  if (mark.length === 2) {
    const x = (mark[0] + 0.5) * k, y = (mark[1] + 0.5) * q;
    g.beginPath(); g.arc(x, y, 8 * d, 0, 2 * Math.PI); g.stroke();
  } else {
    g.strokeRect(mark[0] * k, mark[1] * q, mark[2] * k, mark[3] * q);
  }
}
function box(a, b) {
  const x = Math.min(a[0], b[0]), y = Math.min(a[1], b[1]);
  return [x, y, Math.abs(a[0] - b[0]) + 1, Math.abs(a[1] - b[1]) + 1];
}
function post(line) {
  fetch('/event', {method: 'POST', body: line}).catch(() => {});
}
c.addEventListener('pointerdown', e => { if (!dims()[0]) return; down = toImg(e); c.setPointerCapture(e.pointerId); });
c.addEventListener('pointermove', e => { if (!down) return; mark = box(down, toImg(e)); draw(); });
c.addEventListener('pointerup', e => {
  if (!down) return;
  const p = toImg(e);
  if (Math.hypot(p[2] - down[2], p[3] - down[3]) < 5) {
    mark = [down[0], down[1]]; post('click ' + mark.join(' '));
  } else {
    mark = box(down, p); post('box ' + mark.join(' '));
  }
  down = null; draw();
});
window.addEventListener('resize', draw);
setInterval(() => {
  fetch('/status').then(r => r.json()).then(j => {
    size = [j.w, j.h];
    s.textContent = j.frames ? j.fps.toFixed(1) + ' fps  ' + j.w + 'x' + j.h + '  frames ' + j.frames +
      '  bad ' + j.bad + '  last event: ' + (j.event || '-') : 'waiting for the first frame';
  }).catch(() => { s.textContent = 'omvcon has stopped'; });
}, 500);
</script></body></html>
`
