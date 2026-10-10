// Package upnp talks to UPnP/DLNA media renderers: SSDP discovery,
// AVTransport and RenderingControl.
package upnp

import (
	"context"
	"encoding/xml"
	"fmt"
	"log"
	"net"
	"net/http"
	"net/url"
	"sort"
	"strings"
	"time"
)

const (
	avTransport       = "urn:schemas-upnp-org:service:AVTransport:1"
	renderingControl  = "urn:schemas-upnp-org:service:RenderingControl:1"
	connectionManager = "urn:schemas-upnp-org:service:ConnectionManager:1"
)

var client = &http.Client{Timeout: 10 * time.Second}

// Discover sends an SSDP M-SEARCH for AVTransport and describes every
// device that answers within timeout.
func Discover(ctx context.Context, timeout time.Duration) ([]*Device, error) {
	conn, err := net.ListenUDP("udp4", nil)
	if err != nil {
		return nil, err
	}
	defer conn.Close()

	msg := "M-SEARCH * HTTP/1.1\r\n" +
		"HOST: 239.255.255.250:1900\r\n" +
		"MAN: \"ssdp:discover\"\r\n" +
		"MX: 2\r\n" +
		"ST: " + avTransport + "\r\n\r\n"
	dst := &net.UDPAddr{IP: net.IPv4(239, 255, 255, 250), Port: 1900}
	for range 2 { // UDP is lossy
		if _, err := conn.WriteToUDP([]byte(msg), dst); err != nil {
			return nil, err
		}
	}

	deadline := time.Now().Add(timeout)
	if d, ok := ctx.Deadline(); ok && d.Before(deadline) {
		deadline = d
	}
	conn.SetReadDeadline(deadline)

	seen := map[string]bool{}
	var out []*Device
	buf := make([]byte, 4096)
	for {
		n, _, err := conn.ReadFromUDP(buf)
		if err != nil {
			break // deadline
		}
		loc := header(string(buf[:n]), "LOCATION")
		if loc == "" || seen[loc] {
			continue
		}
		seen[loc] = true
		d, err := describe(ctx, loc)
		if err != nil {
			log.Printf("upnp: skip %s: %v", loc, err)
			continue
		}
		out = append(out, d)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].name < out[j].name })
	return out, nil
}

func header(resp, key string) string {
	for line := range strings.SplitSeq(resp, "\r\n") {
		if k, v, ok := strings.Cut(line, ":"); ok && strings.EqualFold(strings.TrimSpace(k), key) {
			return strings.TrimSpace(v)
		}
	}
	return ""
}

type xmlDevice struct {
	FriendlyName string `xml:"friendlyName"`
	Services     []struct {
		Type       string `xml:"serviceType"`
		ControlURL string `xml:"controlURL"`
	} `xml:"serviceList>service"`
	Devices []xmlDevice `xml:"deviceList>device"`
}

func (d *xmlDevice) has(prefix string) bool {
	for _, s := range d.Services {
		if strings.HasPrefix(s.Type, prefix) {
			return true
		}
	}
	return false
}

// renderer returns the (possibly nested) device that has AVTransport.
func (d *xmlDevice) renderer() *xmlDevice {
	if d.has("urn:schemas-upnp-org:service:AVTransport:") {
		return d
	}
	for i := range d.Devices {
		if r := d.Devices[i].renderer(); r != nil {
			return r
		}
	}
	return nil
}

func describe(ctx context.Context, location string) (*Device, error) {
	req, err := http.NewRequestWithContext(ctx, "GET", location, nil)
	if err != nil {
		return nil, err
	}
	resp, err := client.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()

	var root struct {
		URLBase string    `xml:"URLBase"`
		Device  xmlDevice `xml:"device"`
	}
	if err := xml.NewDecoder(resp.Body).Decode(&root); err != nil {
		return nil, err
	}
	base, err := url.Parse(location)
	if err != nil {
		return nil, err
	}
	if root.URLBase != "" {
		if b, err := url.Parse(root.URLBase); err == nil {
			base = b
		}
	}
	r := root.Device.renderer()
	if r == nil {
		return nil, fmt.Errorf("no AVTransport service")
	}

	d := &Device{name: r.FriendlyName}
	for _, s := range r.Services {
		ref, err := url.Parse(s.ControlURL)
		if err != nil {
			continue
		}
		abs := base.ResolveReference(ref).String()
		switch {
		case strings.HasPrefix(s.Type, "urn:schemas-upnp-org:service:AVTransport:"):
			d.avt = abs
		case strings.HasPrefix(s.Type, "urn:schemas-upnp-org:service:RenderingControl:"):
			d.rc = abs
		case strings.HasPrefix(s.Type, "urn:schemas-upnp-org:service:ConnectionManager:"):
			d.cm = abs
		}
	}
	return d, nil
}
