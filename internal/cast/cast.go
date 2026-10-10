// Package cast is the core of teufelcast. It ties a speaker, the virtual
// sink and the stream server together and watches the speaker while
// streaming. The outside world is reached only through the interfaces below.
package cast

import (
	"context"
	"fmt"
	"log"
	"strings"
	"sync"
	"time"
)

// Speaker is a renderer that can be told to play a URL.
type Speaker interface {
	Name() string
	Host() string
	Play(ctx context.Context, url, mime, title string) error
	Stop(ctx context.Context) error
	TransportState(ctx context.Context) (string, error)
	CurrentURI(ctx context.Context) (string, error)
	Volume(ctx context.Context) (int, error)
	SetVolume(ctx context.Context, v int) error
}

// DiscoverFunc finds the speakers on the network.
type DiscoverFunc func(ctx context.Context) ([]Speaker, error)

// Sink is the virtual output the desktop plays into.
type Sink interface {
	// Acquire creates the sink (optionally making it the default output)
	// and returns a func that undoes it.
	Acquire(switchDefault bool) (release func(), err error)
}

// Stream serves the encoded sink audio to the speaker.
type Stream interface {
	Enable()
	Disable()
	URL(speakerHost string, f Format) (string, error)
	Stats() StreamStats
}

type StreamStats struct {
	Active int    `json:"active"`
	Bytes  int64  `json:"bytes"`
	Remote string `json:"remote"`
}

type Format struct {
	Name string
	MIME string
}

var Formats = []Format{
	{"mp3", "audio/mpeg"},
	{"flac", "audio/flac"},
	{"wav", "audio/wav"},
}

func FormatByName(name string) (Format, bool) {
	for _, f := range Formats {
		if f.Name == name {
			return f, true
		}
	}
	return Format{}, false
}

type Options struct {
	Device        string `json:"device"`
	Format        string `json:"format"`
	SwitchDefault bool   `json:"switchDefault"`
	AutoReconnect bool   `json:"autoReconnect"`
}

type State string

const (
	Idle         State = "idle"
	Starting     State = "starting"
	Streaming    State = "streaming"
	Reconnecting State = "reconnecting"
	Failed       State = "error"
)

type Status struct {
	State State `json:"state"`
	Options
	Speaker   string      `json:"speaker"`
	URL       string      `json:"url"`
	Transport string      `json:"transport"`
	Uptime    float64     `json:"uptime"`
	Stream    StreamStats `json:"stream"`
	LastError string      `json:"lastError"`
	Events    []string    `json:"events"`
}

type session struct {
	opts    Options
	format  Format
	speaker Speaker
	url     string
	release func()
	ctx     context.Context
	cancel  context.CancelFunc
	started time.Time
}

type Service struct {
	discover DiscoverFunc
	sink     Sink
	stream   Stream

	op sync.Mutex // serialises Start, Stop and the watchdog's actions

	mu        sync.Mutex
	state     State
	sess      *session
	opts      Options
	lastErr   string
	transport string
	events    []string

	devMu   sync.Mutex
	devices []Speaker
}

func New(discover DiscoverFunc, sink Sink, stream Stream, defaults Options) *Service {
	return &Service{discover: discover, sink: sink, stream: stream, state: Idle, opts: defaults}
}

// Logf logs and keeps the message for the UI.
func (s *Service) Logf(format string, args ...any) {
	msg := fmt.Sprintf(format, args...)
	log.Print(msg)
	s.mu.Lock()
	defer s.mu.Unlock()
	s.events = append(s.events, time.Now().Format("15:04:05")+" "+msg)
	if len(s.events) > 20 {
		s.events = s.events[len(s.events)-20:]
	}
}

func (s *Service) setState(st State, errMsg string) {
	s.mu.Lock()
	changed := s.state != st
	s.state = st
	if errMsg != "" || st == Idle || st == Starting {
		s.lastErr = errMsg
	}
	s.mu.Unlock()
	if changed {
		s.Logf("state: %s", st)
	}
	if errMsg != "" {
		s.Logf("error: %s", errMsg)
	}
}

// Start streams to opts.Device, stopping a running session first.
func (s *Service) Start(opts Options) error {
	f, ok := FormatByName(opts.Format)
	if !ok {
		return fmt.Errorf("unknown format %q", opts.Format)
	}
	s.op.Lock()
	defer s.op.Unlock()
	if s.sess != nil {
		s.end(s.sess, true, Idle, "")
	}

	s.mu.Lock()
	s.opts, s.transport = opts, ""
	s.mu.Unlock()
	s.setState(Starting, "")
	s.Logf("starting: device=%q format=%s switchDefault=%v autoReconnect=%v",
		opts.Device, opts.Format, opts.SwitchDefault, opts.AutoReconnect)

	ctx, cancel := context.WithCancel(context.Background())
	sess := &session{opts: opts, format: f, ctx: ctx, cancel: cancel}
	if err := s.begin(sess); err != nil {
		cancel()
		s.setState(Failed, err.Error())
		return err
	}
	sess.started = time.Now()
	s.mu.Lock()
	s.sess = sess
	s.mu.Unlock()
	s.setState(Streaming, "")
	go s.watch(sess)
	return nil
}

func (s *Service) begin(sess *session) error {
	sp, err := s.find(sess.ctx, sess.opts.Device, false)
	if err != nil {
		return err
	}
	sess.speaker = sp
	release, err := s.sink.Acquire(sess.opts.SwitchDefault)
	if err != nil {
		return err
	}
	s.stream.Enable()
	if err := s.load(sess); err != nil {
		s.stream.Disable()
		release()
		return err
	}
	sess.release = release
	return nil
}

// load tells the speaker to play our stream. If that fails the speaker is
// rediscovered once, since its ports change after a reboot.
func (s *Service) load(sess *session) error {
	err := s.play(sess)
	if err == nil || sess.ctx.Err() != nil {
		return err
	}
	s.Logf("play failed (%v), rediscovering %q", err, sess.opts.Device)
	sp, ferr := s.find(sess.ctx, sess.opts.Device, true)
	if ferr != nil {
		return fmt.Errorf("%w (rediscovery: %v)", err, ferr)
	}
	s.mu.Lock()
	sess.speaker = sp
	s.mu.Unlock()
	return s.play(sess)
}

func (s *Service) play(sess *session) error {
	url, err := s.stream.URL(sess.speaker.Host(), sess.format)
	if err != nil {
		return err
	}
	s.mu.Lock()
	sess.url = url
	s.mu.Unlock()
	s.Logf("telling %q to play %s", sess.speaker.Name(), url)
	return sess.speaker.Play(sess.ctx, url, sess.format.MIME, "Desktop audio")
}

// Stop stops the speaker and tears everything down.
func (s *Service) Stop() {
	s.op.Lock()
	defer s.op.Unlock()
	if s.sess == nil {
		s.mu.Lock()
		failed := s.state == Failed
		s.mu.Unlock()
		if failed {
			s.setState(Idle, "")
		}
		return
	}
	s.Logf("stopping")
	s.end(s.sess, true, Idle, "")
}

// end tears sess down. Caller holds s.op.
func (s *Service) end(sess *session, stopSpeaker bool, final State, msg string) {
	sess.cancel()
	if stopSpeaker {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		if err := sess.speaker.Stop(ctx); err != nil {
			s.Logf("speaker stop: %v", err)
		}
		cancel()
	}
	s.stream.Disable()
	sess.release()
	s.mu.Lock()
	if s.sess == sess {
		s.sess = nil
	}
	s.mu.Unlock()
	s.setState(final, msg)
}

// Devices returns the names of the known speakers.
func (s *Service) Devices(ctx context.Context, refresh bool) ([]string, error) {
	s.devMu.Lock()
	defer s.devMu.Unlock()
	if refresh || len(s.devices) == 0 {
		if err := s.refresh(ctx); err != nil {
			return nil, err
		}
	}
	names := make([]string, len(s.devices))
	for i, d := range s.devices {
		names[i] = d.Name()
	}
	return names, nil
}

func (s *Service) find(ctx context.Context, name string, fresh bool) (Speaker, error) {
	s.devMu.Lock()
	defer s.devMu.Unlock()
	if !fresh {
		if sp := pick(s.devices, name); sp != nil {
			return sp, nil
		}
	}
	if err := s.refresh(ctx); err != nil {
		return nil, err
	}
	if sp := pick(s.devices, name); sp != nil {
		return sp, nil
	}
	return nil, fmt.Errorf("no speaker matching %q found", name)
}

// refresh rediscovers the speakers. Caller holds s.devMu.
func (s *Service) refresh(ctx context.Context) error {
	ds, err := s.discover(ctx)
	if err != nil {
		return err
	}
	s.devices = ds
	names := make([]string, len(ds))
	for i, d := range ds {
		names[i] = d.Name()
	}
	log.Printf("discovery: %d speaker(s): %s", len(ds), strings.Join(names, ", "))
	return nil
}

// pick prefers an exact (case-insensitive) name match over a substring match.
func pick(ds []Speaker, name string) Speaker {
	for _, d := range ds {
		if strings.EqualFold(d.Name(), name) {
			return d
		}
	}
	for _, d := range ds {
		if strings.Contains(strings.ToLower(d.Name()), strings.ToLower(name)) {
			return d
		}
	}
	return nil
}

// speaker returns the streaming speaker if it matches name, else looks it up.
func (s *Service) speaker(ctx context.Context, name string) (Speaker, error) {
	s.mu.Lock()
	sess := s.sess
	if name == "" {
		name = s.opts.Device
	}
	var sp Speaker
	if sess != nil && (name == sess.opts.Device || name == sess.speaker.Name()) {
		sp = sess.speaker
	}
	s.mu.Unlock()
	if sp != nil {
		return sp, nil
	}
	return s.find(ctx, name, false)
}

func (s *Service) Volume(ctx context.Context, device string) (int, error) {
	sp, err := s.speaker(ctx, device)
	if err != nil {
		return 0, err
	}
	return sp.Volume(ctx)
}

func (s *Service) SetVolume(ctx context.Context, device string, v int) error {
	sp, err := s.speaker(ctx, device)
	if err != nil {
		return err
	}
	return sp.SetVolume(ctx, max(0, min(100, v)))
}

func (s *Service) Status() Status {
	stats := s.stream.Stats()
	s.mu.Lock()
	defer s.mu.Unlock()
	st := Status{
		State: s.state, Options: s.opts, Transport: s.transport,
		Stream: stats, LastError: s.lastErr,
		Events: append([]string(nil), s.events...),
	}
	if sess := s.sess; sess != nil {
		st.Speaker, st.URL = sess.speaker.Name(), sess.url
		st.Uptime = time.Since(sess.started).Seconds()
	}
	return st
}
