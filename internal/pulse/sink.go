// Package pulse manages the "teufel" null sink through pactl.
package pulse

import (
	"fmt"
	"log"
	"os/exec"
	"strings"
)

const (
	SinkName = "teufel"
	// Monitor is the PulseAudio-compat name of the sink's monitor source.
	// PipeWire-native tools don't know it; pactl, parec and ffmpeg -f pulse do.
	Monitor = SinkName + ".monitor"
)

type Sink struct{}

// Acquire creates the null sink unless it already exists (e.g. left over
// from a crash) and optionally makes it the default output. The returned
// func restores the default and removes the sink if we created it.
func (Sink) Acquire(switchDefault bool) (func(), error) {
	exists, err := sinkExists()
	if err != nil {
		return nil, err
	}
	module := ""
	if exists {
		log.Printf("pulse: reusing existing sink %q", SinkName)
	} else {
		module, err = pactl("load-module", "module-null-sink",
			"sink_name="+SinkName, "sink_properties=device.description=Teufel")
		if err != nil {
			return nil, err
		}
		log.Printf("pulse: created sink %q (module %s)", SinkName, module)
	}

	prev := ""
	if switchDefault {
		cur, err := pactl("get-default-sink")
		if err == nil && cur != SinkName {
			_, err = pactl("set-default-sink", SinkName)
		}
		if err != nil {
			unload(module)
			return nil, err
		}
		if cur != SinkName {
			prev = cur
			log.Printf("pulse: default output %s -> %s", cur, SinkName)
		}
	}

	return func() {
		// Only restore if the user didn't pick something else meanwhile.
		if cur, err := pactl("get-default-sink"); prev != "" && err == nil && cur == SinkName {
			if _, err := pactl("set-default-sink", prev); err != nil {
				log.Printf("pulse: %v", err)
			} else {
				log.Printf("pulse: default output restored to %s", prev)
			}
		}
		unload(module)
	}, nil
}

func unload(module string) {
	if module == "" {
		return
	}
	if _, err := pactl("unload-module", module); err != nil {
		log.Printf("pulse: %v", err)
		return
	}
	log.Printf("pulse: removed sink %q (module %s)", SinkName, module)
}

func sinkExists() (bool, error) {
	out, err := pactl("list", "short", "sinks")
	if err != nil {
		return false, err
	}
	for line := range strings.SplitSeq(out, "\n") {
		if f := strings.Fields(line); len(f) >= 2 && f[1] == SinkName {
			return true, nil
		}
	}
	return false, nil
}

func pactl(args ...string) (string, error) {
	out, err := exec.Command("pactl", args...).CombinedOutput()
	s := strings.TrimSpace(string(out))
	if err != nil {
		return "", fmt.Errorf("pactl %s: %v: %s", strings.Join(args, " "), err, s)
	}
	return s, nil
}
