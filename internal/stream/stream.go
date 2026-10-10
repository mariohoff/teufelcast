// Package stream serves the sink's audio over HTTP. Every GET runs its own
// ffmpeg that captures the monitor and encodes it live.
package stream

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"os/exec"
	"strconv"
	"strings"
	"sync"
	"time"

	"teufelcast/internal/cast"
	"teufelcast/internal/pulse"
	"teufelcast/internal/upnp"
)

var codecArgs = map[string][]string{
	"mp3":  {"-c:a", "libmp3lame", "-b:a", "320k", "-f", "mp3"},
	"flac": {"-c:a", "flac", "-f", "flac"},
	"wav":  {"-c:a", "pcm_s16le", "-f", "wav"},
}

type Server struct {
	port int
	logf func(string, ...any)

	mu      sync.Mutex
	enabled bool
	conns   map[*conn]struct{}
	bytes   int64
	remote  string
}

type conn struct {
	cancel context.CancelFunc
}

func New(port int, logf func(string, ...any)) *Server {
	return &Server{port: port, logf: logf, conns: map[*conn]struct{}{}}
}

// Enable starts serving all formats.
func (s *Server) Enable() {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.enabled, s.bytes, s.remote = true, 0, ""
}

// Disable stops serving and kills every running ffmpeg.
func (s *Server) Disable() {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.enabled = false
	for c := range s.conns {
		c.cancel()
	}
}

// URL is the stream URL as seen from speakerHost.
func (s *Server) URL(speakerHost string, f cast.Format) (string, error) {
	c, err := net.Dial("udp4", net.JoinHostPort(speakerHost, "1900"))
	if err != nil {
		return "", err
	}
	defer c.Close()
	ip := c.LocalAddr().(*net.UDPAddr).IP.String()
	return "http://" + net.JoinHostPort(ip, strconv.Itoa(s.port)) + "/live." + f.Name, nil
}

func (s *Server) Stats() cast.StreamStats {
	s.mu.Lock()
	defer s.mu.Unlock()
	return cast.StreamStats{Active: len(s.conns), Bytes: s.bytes, Remote: s.remote}
}

func (s *Server) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	log.Printf("http: %s %s from %s range=%q ua=%q",
		r.Method, r.URL.Path, r.RemoteAddr, r.Header.Get("Range"), r.Header.Get("User-Agent"))

	f, ok := cast.FormatByName(strings.TrimPrefix(r.URL.Path, "/live."))
	if !ok || !strings.HasPrefix(r.URL.Path, "/live.") {
		http.NotFound(w, r)
		return
	}
	if r.Method != http.MethodGet && r.Method != http.MethodHead {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	s.mu.Lock()
	enabled := s.enabled
	s.mu.Unlock()
	if !enabled {
		http.Error(w, "not streaming", http.StatusServiceUnavailable)
		return
	}

	h := w.Header()
	h.Set("Content-Type", f.MIME)
	h.Set("transferMode.dlna.org", "Streaming")
	h.Set("contentFeatures.dlna.org", upnp.ContentFeatures(f.MIME))
	h.Set("Cache-Control", "no-cache")
	if r.Method == http.MethodHead {
		return
	}

	ctx, cancel := context.WithCancel(r.Context())
	defer cancel()
	args := []string{"-hide_banner", "-loglevel", "error", "-nostdin",
		"-fflags", "nobuffer", "-f", "pulse", "-i", pulse.Monitor,
		"-ac", "2", "-ar", "44100"}
	args = append(args, codecArgs[f.Name]...)
	args = append(args, "-flush_packets", "1", "pipe:1")
	cmd := exec.CommandContext(ctx, "ffmpeg", args...)
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	out, err := cmd.StdoutPipe()
	if err == nil {
		err = cmd.Start()
	}
	if err != nil {
		s.logf("stream: ffmpeg: %v", err)
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}

	c := &conn{cancel: cancel}
	s.mu.Lock()
	s.conns[c] = struct{}{}
	s.remote = r.RemoteAddr
	s.mu.Unlock()
	s.logf("speaker connected: %s %s", r.RemoteAddr, r.URL.Path)

	start := time.Now()
	n, copyErr := s.copy(w, out)
	stopped := ctx.Err() != nil && r.Context().Err() == nil // Disable() cancelled us
	cancel()
	waitErr := cmd.Wait()

	s.mu.Lock()
	delete(s.conns, c)
	s.mu.Unlock()

	why := "client closed"
	switch {
	case stopped:
		why = "stopped"
	case r.Context().Err() == nil && copyErr == nil:
		why = fmt.Sprintf("ffmpeg exited: %v %s", waitErr, strings.TrimSpace(stderr.String()))
	}
	s.logf("speaker disconnected: %s after %s, %d KiB (%s)",
		r.RemoteAddr, time.Since(start).Round(time.Second), n>>10, why)
}

// copy streams src to w, flushing after every read so nothing sits in a buffer.
func (s *Server) copy(w http.ResponseWriter, src io.Reader) (int64, error) {
	rc := http.NewResponseController(w)
	buf := make([]byte, 8192)
	var total int64
	for {
		n, err := src.Read(buf)
		if n > 0 {
			if _, err := w.Write(buf[:n]); err != nil {
				return total, err
			}
			if err := rc.Flush(); err != nil {
				return total, err
			}
			total += int64(n)
			s.mu.Lock()
			s.bytes += int64(n)
			s.mu.Unlock()
		}
		if err == io.EOF {
			return total, nil
		}
		if err != nil {
			return total, err
		}
	}
}
