package upnp

import (
	"context"
	"encoding/xml"
	"fmt"
	"html"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
)

// Device is a media renderer.
type Device struct {
	name string
	avt  string // AVTransport control URL
	rc   string // RenderingControl control URL, may be empty
	cm   string // ConnectionManager control URL, may be empty
}

func (d *Device) Name() string { return d.name }

// ControlURL is the AVTransport control URL.
func (d *Device) ControlURL() string { return d.avt }

// Accepts returns the protocolInfo entries the renderer can play,
// e.g. "http-get:*:audio/mpeg:*".
func (d *Device) Accepts(ctx context.Context) ([]string, error) {
	if d.cm == "" {
		return nil, fmt.Errorf("%s has no ConnectionManager", d.name)
	}
	body, err := soap(ctx, d.cm, connectionManager, "GetProtocolInfo", "")
	if err != nil {
		return nil, err
	}
	var out []string
	for p := range strings.SplitSeq(xmlValue(body, "Sink"), ",") {
		if p = strings.TrimSpace(p); p != "" {
			out = append(out, p)
		}
	}
	return out, nil
}

// Host is the renderer's hostname or IP.
func (d *Device) Host() string {
	u, err := url.Parse(d.avt)
	if err != nil {
		return ""
	}
	return u.Hostname()
}

// Play loads a live stream and starts playback. If the renderer rejects the
// metadata it tries once more without.
func (d *Device) Play(ctx context.Context, uri, mime, title string) error {
	_ = d.Stop(ctx) // fails when already stopped
	if err := d.setURI(ctx, uri, didl(title, uri, mime)); err != nil {
		if err := d.setURI(ctx, uri, ""); err != nil {
			return err
		}
	}
	_, err := d.avtCall(ctx, "Play", "<Speed>1</Speed>")
	return err
}

func (d *Device) Stop(ctx context.Context) error {
	_, err := d.avtCall(ctx, "Stop", "")
	return err
}

// TransportState returns PLAYING, STOPPED, TRANSITIONING, NO_MEDIA_PRESENT etc.
func (d *Device) TransportState(ctx context.Context) (string, error) {
	body, err := d.avtCall(ctx, "GetTransportInfo", "")
	if err != nil {
		return "", err
	}
	return xmlValue(body, "CurrentTransportState"), nil
}

func (d *Device) CurrentURI(ctx context.Context) (string, error) {
	body, err := d.avtCall(ctx, "GetMediaInfo", "")
	if err != nil {
		return "", err
	}
	return strings.TrimSpace(xmlValue(body, "CurrentURI")), nil
}

func (d *Device) Volume(ctx context.Context) (int, error) {
	body, err := d.rcCall(ctx, "GetVolume", "")
	if err != nil {
		return 0, err
	}
	return strconv.Atoi(strings.TrimSpace(xmlValue(body, "CurrentVolume")))
}

func (d *Device) SetVolume(ctx context.Context, v int) error {
	_, err := d.rcCall(ctx, "SetVolume", "<DesiredVolume>"+strconv.Itoa(v)+"</DesiredVolume>")
	return err
}

func (d *Device) setURI(ctx context.Context, uri, meta string) error {
	_, err := d.avtCall(ctx, "SetAVTransportURI",
		"<CurrentURI>"+html.EscapeString(uri)+"</CurrentURI>"+
			"<CurrentURIMetaData>"+html.EscapeString(meta)+"</CurrentURIMetaData>")
	return err
}

func (d *Device) avtCall(ctx context.Context, action, args string) (string, error) {
	return soap(ctx, d.avt, avTransport, action, "<InstanceID>0</InstanceID>"+args)
}

func (d *Device) rcCall(ctx context.Context, action, args string) (string, error) {
	if d.rc == "" {
		return "", fmt.Errorf("%s has no RenderingControl", d.name)
	}
	return soap(ctx, d.rc, renderingControl, action, "<InstanceID>0</InstanceID><Channel>Master</Channel>"+args)
}

func soap(ctx context.Context, controlURL, service, action, args string) (string, error) {
	body := `<?xml version="1.0" encoding="utf-8"?>` +
		`<s:Envelope xmlns:s="http://schemas.xmlsoap.org/soap/envelope/" s:encodingStyle="http://schemas.xmlsoap.org/soap/encoding/">` +
		`<s:Body><u:` + action + ` xmlns:u="` + service + `">` + args +
		`</u:` + action + `></s:Body></s:Envelope>`
	req, err := http.NewRequestWithContext(ctx, "POST", controlURL, strings.NewReader(body))
	if err != nil {
		return "", err
	}
	req.Header.Set("Content-Type", `text/xml; charset="utf-8"`)
	req.Header.Set("SOAPAction", `"`+service+`#`+action+`"`)
	resp, err := client.Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != http.StatusOK {
		msg := strings.TrimSpace(xmlValue(string(b), "errorCode") + " " + xmlValue(string(b), "errorDescription"))
		if msg == "" {
			msg = strings.TrimSpace(string(b))
		}
		return "", fmt.Errorf("%s: HTTP %d: %s", action, resp.StatusCode, msg)
	}
	return string(b), nil
}

// xmlValue returns the text of the first element named tag.
func xmlValue(doc, tag string) string {
	dec := xml.NewDecoder(strings.NewReader(doc))
	for {
		tok, err := dec.Token()
		if err != nil {
			return ""
		}
		if se, ok := tok.(xml.StartElement); ok && se.Name.Local == tag {
			var s string
			if dec.DecodeElement(&s, &se) != nil {
				return ""
			}
			return s
		}
	}
}

// ContentFeatures is the DLNA 4th protocolInfo field for a live stream:
// no seeking (OP=00), streaming transfer mode. The stream server sends it as
// the contentFeatures.dlna.org header.
func ContentFeatures(mime string) string {
	pn := ""
	if mime == "audio/mpeg" {
		pn = "DLNA.ORG_PN=MP3;"
	}
	return pn + "DLNA.ORG_OP=00;DLNA.ORG_CI=0;DLNA.ORG_FLAGS=01700000000000000000000000000000"
}

// didl is minimal DIDL-Lite metadata; many renderers refuse an empty one.
func didl(title, uri, mime string) string {
	return `<DIDL-Lite xmlns="urn:schemas-upnp-org:metadata-1-0/DIDL-Lite/" ` +
		`xmlns:dc="http://purl.org/dc/elements/1.1/" ` +
		`xmlns:upnp="urn:schemas-upnp-org:metadata-1-0/upnp/">` +
		`<item id="1" parentID="0" restricted="1">` +
		`<dc:title>` + html.EscapeString(title) + `</dc:title>` +
		`<upnp:class>object.item.audioItem.audioBroadcast</upnp:class>` +
		`<res protocolInfo="http-get:*:` + mime + `:` + ContentFeatures(mime) + `">` +
		html.EscapeString(uri) + `</res></item></DIDL-Lite>`
}
