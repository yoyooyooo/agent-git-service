package edge

import (
	"bytes"
	"errors"
	"fmt"
	"net/http"
	"strings"

	"github.com/ngaut/agent-git-service/internal/gitbackend"
)

// Capability advertisement is small and bounded. Filter only v2 capabilities
// that this adapter actually handles; do not advertise object-info, filters,
// ref-in-want or packfile-uris and then silently delegate a different protocol.
func serveReadCapabilities(w http.ResponseWriter, r *http.Request, backend gitbackend.Request) error {
	capture := &capabilityCapture{header: make(http.Header)}
	if err := gitbackend.Serve(capture, r, backend); err != nil {
		return err
	}
	if capture.err != nil || capture.status != http.StatusOK {
		return errors.New("native capability advertisement failed")
	}
	packets, err := readPackets(capture.body.Bytes())
	if err != nil || len(packets) < 2 || packets[0].line != "version 2" || packets[len(packets)-1].kind != 0 {
		return errors.New("native backend did not negotiate protocol v2")
	}
	var out bytes.Buffer
	writePacket := func(line string) { fmt.Fprintf(&out, "%04x%s\n", len(line)+5, line) }
	writePacket("version 2")
	for _, pkt := range packets[1 : len(packets)-1] {
		if pkt.kind != -1 {
			return errors.New("invalid native capability framing")
		}
		key, value, _ := strings.Cut(pkt.line, "=")
		switch key {
		case "agent", "object-format", "server-option":
			writePacket(pkt.line)
		case "ls-refs", "fetch":
			var features []string
			for _, feature := range strings.Fields(value) {
				if key == "ls-refs" && feature == "unborn" || key == "fetch" && (feature == "shallow" || feature == "wait-for-done") {
					features = append(features, feature)
				}
			}
			if len(features) == 0 {
				writePacket(key)
			} else {
				writePacket(key + "=" + strings.Join(features, " "))
			}
		}
	}
	out.WriteString("0000")
	w.Header().Set("Content-Type", "application/x-git-upload-pack-advertisement")
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Del("Content-Length")
	w.WriteHeader(http.StatusOK)
	_, err = w.Write(out.Bytes())
	return err
}

type capabilityCapture struct {
	header http.Header
	status int
	body   bytes.Buffer
	err    error
}

func (c *capabilityCapture) Header() http.Header { return c.header }
func (c *capabilityCapture) WriteHeader(status int) {
	if c.status == 0 {
		c.status = status
	}
}
func (c *capabilityCapture) Write(p []byte) (int, error) {
	if c.status == 0 {
		c.WriteHeader(http.StatusOK)
	}
	if len(p) > (64<<10)-c.body.Len() {
		c.err = errors.New("capability advertisement exceeds limit")
		return 0, c.err
	}
	return c.body.Write(p)
}
