package cast

import (
	"fmt"
	"net/url"
	"strings"
	"time"
)

const (
	watchInterval  = 5 * time.Second
	watchGrace     = 10 * time.Second // give the speaker time to connect after Play
	maxReconnects  = 5
	reconnectDelay = 5 * time.Second // doubled per attempt, capped at a minute
)

// watch polls the speaker while sess runs. If someone else takes the speaker
// over it bows out. If playback stopped and nobody is connected it re-sends
// the stream, with backoff.
func (s *Service) watch(sess *session) {
	t := time.NewTicker(watchInterval)
	defer t.Stop()
	grace := time.Now().Add(watchGrace)
	var nextTry time.Time
	bad, attempts := 0, 0
	lastErr := ""

	for {
		select {
		case <-sess.ctx.Done():
			return
		case <-t.C:
		}

		ts, err := sess.speaker.TransportState(sess.ctx)
		if sess.ctx.Err() != nil {
			return
		}
		if err != nil {
			if err.Error() != lastErr {
				s.Logf("watchdog: %v", err)
				lastErr = err.Error()
			}
			ts = "UNREACHABLE"
		} else {
			lastErr = ""
			uri, err := sess.speaker.CurrentURI(sess.ctx)
			if err == nil && uri != "" && !sameStream(uri, sess.url) {
				s.Logf("speaker now plays %s", uri)
				s.endFromWatch(sess, false, Idle, "speaker taken over by another app")
				return
			}
		}
		s.mu.Lock()
		changed := s.transport != ts
		s.transport = ts
		state := s.state
		s.mu.Unlock()
		if changed {
			s.Logf("speaker: %s", ts)
		}

		active := s.stream.Stats().Active
		stopped := ts == "STOPPED" || ts == "NO_MEDIA_PRESENT" || ts == "UNREACHABLE"
		if !stopped || active > 0 {
			bad = 0
			if active > 0 && ts == "PLAYING" && state == Reconnecting {
				attempts = 0
				s.setState(Streaming, "")
			}
			continue
		}
		if time.Now().Before(grace) {
			continue
		}
		if bad++; bad < 2 { // states flicker, want two in a row
			continue
		}
		if !sess.opts.AutoReconnect {
			s.endFromWatch(sess, false, Idle, "speaker stopped playing")
			return
		}
		if attempts >= maxReconnects {
			s.endFromWatch(sess, true, Failed, fmt.Sprintf("gave up after %d reconnect attempts", attempts))
			return
		}
		if time.Now().Before(nextTry) {
			continue
		}
		attempts++
		s.setState(Reconnecting, "")
		s.Logf("reconnect %d/%d (speaker %s, nobody connected)", attempts, maxReconnects, ts)
		if !s.reload(sess) {
			return
		}
		nextTry = time.Now().Add(min(reconnectDelay<<(attempts-1), time.Minute))
		grace = time.Now().Add(watchGrace)
		bad = 0
	}
}

// reload re-sends the stream unless sess ended meanwhile.
func (s *Service) reload(sess *session) bool {
	s.op.Lock()
	defer s.op.Unlock()
	if s.sess != sess || sess.ctx.Err() != nil {
		return false
	}
	if err := s.load(sess); err != nil {
		s.Logf("reconnect failed: %v", err)
		s.mu.Lock()
		s.lastErr = err.Error()
		s.mu.Unlock()
	}
	return true
}

// endFromWatch ends sess unless the user already did.
func (s *Service) endFromWatch(sess *session, stopSpeaker bool, final State, msg string) {
	s.op.Lock()
	defer s.op.Unlock()
	if s.sess == sess {
		s.end(sess, stopSpeaker, final, msg)
	}
}

// sameStream compares loosely, renderers may normalise the URI.
func sameStream(a, b string) bool {
	if a == b {
		return true
	}
	ua, err1 := url.Parse(a)
	ub, err2 := url.Parse(b)
	return err1 == nil && err2 == nil && ua.Host == ub.Host && strings.HasPrefix(ua.Path, "/live")
}
