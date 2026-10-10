// teufelcast streams the desktop's audio to a UPnP/DLNA speaker.
//
// Apps play into the PipeWire null sink "teufel". The speaker is told to
// fetch http://<this-host>:8090/live.mp3, and every such request gets an
// ffmpeg that encodes the sink's monitor live. A web UI on 127.0.0.1:8091
// starts and stops it.
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"log"
	"net"
	"net/http"
	"os"
	"os/exec"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"teufelcast/internal/cast"
	"teufelcast/internal/pulse"
	"teufelcast/internal/stream"
	"teufelcast/internal/upnp"
	"teufelcast/internal/web"
)

func main() {
	uiAddr := flag.String("ui-addr", "127.0.0.1:8091", "web UI address")
	streamPort := flag.Int("stream-port", 8090, "port the speaker fetches the stream from")
	device := flag.String("device", "Coolzimmer", "speaker name")
	format := flag.String("format", "mp3", "default format: mp3, flac or wav")
	list := flag.Bool("list", false, "list speakers and exit")
	formats := flag.Bool("formats", false, "list what -device accepts and exit")
	flag.Parse()

	if *list || *formats {
		if err := debug(*device, *formats); err != nil {
			log.Fatal(err)
		}
		return
	}

	if _, ok := cast.FormatByName(*format); !ok {
		log.Fatalf("unknown format %q", *format)
	}
	if err := checkDeps(); err != nil {
		log.Fatal(err)
	}

	discover := func(ctx context.Context) ([]cast.Speaker, error) {
		ds, err := upnp.Discover(ctx, 3*time.Second)
		out := make([]cast.Speaker, len(ds))
		for i, d := range ds {
			out[i] = d
		}
		return out, err
	}

	var svc *cast.Service
	streamer := stream.New(*streamPort, func(f string, a ...any) { svc.Logf(f, a...) })
	svc = cast.New(discover, pulse.Sink{}, streamer, cast.Options{
		Device: *device, Format: *format, SwitchDefault: true, AutoReconnect: true,
	})

	// The stream server has no write timeout: the body never ends.
	streamSrv := &http.Server{Addr: fmt.Sprintf(":%d", *streamPort), Handler: streamer, ReadHeaderTimeout: 10 * time.Second}
	uiSrv := &http.Server{Addr: *uiAddr, Handler: web.Handler(svc), ReadHeaderTimeout: 10 * time.Second}
	for _, srv := range []*http.Server{streamSrv, uiSrv} {
		ln, err := net.Listen("tcp", srv.Addr)
		if err != nil {
			log.Fatal(err)
		}
		go func() {
			if err := srv.Serve(ln); !errors.Is(err, http.ErrServerClosed) {
				log.Fatal(err)
			}
		}()
	}
	log.Printf("streaming on :%d, UI on http://%s", *streamPort, *uiAddr)

	go svc.Devices(context.Background(), true)

	sig := make(chan os.Signal, 1)
	signal.Notify(sig, os.Interrupt, syscall.SIGTERM)
	<-sig
	signal.Stop(sig) // a second Ctrl+C kills us
	log.Print("shutting down")
	svc.Stop()
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	uiSrv.Shutdown(ctx)
	streamSrv.Shutdown(ctx)
}

// debug prints the speakers found and, with formats, what device accepts.
func debug(device string, formats bool) error {
	ctx := context.Background()
	ds, err := upnp.Discover(ctx, 3*time.Second)
	if err != nil {
		return err
	}
	var target *upnp.Device
	for _, d := range ds {
		fmt.Printf("%-30s %s\n", d.Name(), d.ControlURL())
		if strings.EqualFold(d.Name(), device) {
			target = d
		}
	}
	if !formats {
		return nil
	}
	if target == nil {
		return fmt.Errorf("no speaker named %q", device)
	}
	accepts, err := target.Accepts(ctx)
	if err != nil {
		return err
	}
	fmt.Printf("\n%s accepts:\n", target.Name())
	for _, p := range accepts {
		fmt.Println("  " + p)
	}
	return nil
}

func checkDeps() error {
	for _, bin := range []string{"ffmpeg", "pactl"} {
		if _, err := exec.LookPath(bin); err != nil {
			return fmt.Errorf("%s not found in PATH (dnf install ffmpeg-free pulseaudio-utils)", bin)
		}
	}
	out, err := exec.Command("ffmpeg", "-hide_banner", "-encoders").Output()
	if err != nil {
		return fmt.Errorf("ffmpeg -encoders: %v", err)
	}
	if !strings.Contains(string(out), " libmp3lame ") {
		return errors.New("ffmpeg lacks libmp3lame; ffmpeg-free normally has it, else use ffmpeg from RPM Fusion")
	}
	return nil
}
