package edge

import (
	"bytes"
	"compress/gzip"
	"errors"
	"io"
	"mime"
	"net/http"
	"strconv"
	"strings"
)

const maxReadRequestBytes = 8 << 20
const maxReadPackets = 65536
const maxReadWants = 4096

var errReadRequest = errors.New("invalid or unsupported Git read request")
var errReadBodyLimit = errors.New("Git read request exceeds limit")

// gitReadRequest classifies one stateless RPC. Original packet bytes (after
// bounded HTTP content decoding) are passed unchanged to native Git. It never
// produces a pack, interprets credentials or pins a client session by IP/cookie.
type gitReadRequest struct {
	version int
	phase   string
	body    []byte
	wants   []string
}

type gitPacket struct {
	kind int // -1=data, 0=flush, 1=delimiter
	line string
}

func parseReadRequest(r *http.Request) (gitReadRequest, error) {
	out := gitReadRequest{phase: "discover"}
	if values := r.Header.Values("Git-Protocol"); len(values) > 1 {
		return out, errReadRequest
	}
	switch r.Header.Get("Git-Protocol") {
	case "", "version=0":
	case "version=1":
		out.version = 1
	case "version=2":
		out.version = 2
	default:
		return out, errReadRequest
	}
	if r.Method == http.MethodGet {
		if r.ContentLength > 0 || len(r.TransferEncoding) != 0 || r.Header.Get("Content-Encoding") != "" {
			return out, errReadRequest
		}
		return out, nil
	}
	media, params, err := mime.ParseMediaType(r.Header.Get("Content-Type"))
	if err != nil || media != "application/x-git-upload-pack-request" || len(params) != 0 || r.Method != http.MethodPost || len(r.Header.Values("Content-Encoding")) > 1 {
		return out, errReadRequest
	}
	if r.Body == nil {
		return out, errReadRequest
	}
	defer r.Body.Close()
	raw, err := io.ReadAll(io.LimitReader(r.Body, maxReadRequestBytes+1))
	if err != nil {
		return out, errReadRequest
	}
	if len(raw) > maxReadRequestBytes {
		return out, errReadBodyLimit
	}
	switch r.Header.Get("Content-Encoding") {
	case "", "identity":
		out.body = raw
	case "gzip":
		compressed := bytes.NewReader(raw)
		z, err := gzip.NewReader(compressed)
		if err != nil {
			return out, errReadRequest
		}
		z.Multistream(false)
		out.body, err = io.ReadAll(io.LimitReader(z, maxReadRequestBytes+1))
		closeErr := z.Close()
		if len(out.body) > maxReadRequestBytes {
			return out, errReadBodyLimit
		}
		if err != nil || closeErr != nil || compressed.Len() != 0 {
			return out, errReadRequest
		}
	default:
		return out, errReadRequest
	}
	packets, err := readPackets(out.body)
	if err != nil {
		return out, err
	}
	if out.version == 2 {
		err = out.parseV2(packets)
	} else {
		err = out.parseV0(packets)
	}
	return out, err
}

func readPackets(body []byte) ([]gitPacket, error) {
	packets := make([]gitPacket, 0, 32)
	for len(body) != 0 {
		if len(body) < 4 || len(packets) >= maxReadPackets {
			return nil, errReadRequest
		}
		for _, b := range body[:4] {
			if !(b >= '0' && b <= '9' || b >= 'a' && b <= 'f' || b >= 'A' && b <= 'F') {
				return nil, errReadRequest
			}
		}
		n, err := strconv.ParseUint(string(body[:4]), 16, 16)
		if err != nil || n == 2 || n == 3 || n == 4 || n > 65520 || n > 1 && int(n) > len(body) {
			return nil, errReadRequest
		}
		if n <= 1 {
			packets = append(packets, gitPacket{kind: int(n)})
			body = body[4:]
			continue
		}
		line := strings.TrimSuffix(string(body[4:n]), "\n")
		if line == "" || strings.ContainsAny(line, "\x00\r\n") {
			return nil, errReadRequest
		}
		packets = append(packets, gitPacket{kind: -1, line: line})
		body = body[n:]
	}
	if len(packets) == 0 {
		return nil, errReadRequest
	}
	return packets, nil
}

func readOID(oid string) bool {
	if (len(oid) != 40 && len(oid) != 64) || strings.Trim(oid, "0") == "" {
		return false
	}
	return strings.Trim(oid, "0123456789abcdef") == ""
}

func (out *gitReadRequest) addWant(oid string) error {
	if !readOID(oid) || len(out.wants) >= maxReadWants || len(out.wants) > 0 && len(oid) != len(out.wants[0]) {
		return errReadRequest
	}
	out.wants = append(out.wants, oid)
	return nil
}

func (out *gitReadRequest) parseV2(p []gitPacket) error {
	// An empty request terminates negotiation; it does not select an old view.
	if len(p) == 1 && p[0].kind == 0 {
		return nil
	}
	if len(p) < 3 || p[0].kind != -1 || p[len(p)-1].kind != 0 {
		return errReadRequest
	}
	command := p[0].line
	if command != "command=ls-refs" && command != "command=fetch" {
		return errReadRequest
	}
	if command == "command=fetch" {
		out.phase = "fetch"
	}
	i := 1
	for ; i < len(p) && p[i].kind == -1; i++ {
		line := p[i].line
		if !(strings.HasPrefix(line, "agent=") || strings.HasPrefix(line, "object-format=") || strings.HasPrefix(line, "server-option=")) {
			return errReadRequest
		}
	}
	if i >= len(p)-1 || p[i].kind != 1 {
		return errReadRequest
	}
	for _, arg := range p[i+1 : len(p)-1] {
		if arg.kind != -1 {
			return errReadRequest
		}
		line := arg.line
		if out.phase == "discover" {
			if line != "peel" && line != "symrefs" && line != "unborn" && !strings.HasPrefix(line, "ref-prefix ") {
				return errReadRequest
			}
			continue
		}
		if strings.HasPrefix(line, "want ") {
			if err := out.addWant(strings.TrimPrefix(line, "want ")); err != nil {
				return err
			}
			continue
		}
		if !validFetchArgument(line) {
			return errReadRequest
		}
	}
	if out.phase == "fetch" && len(out.wants) == 0 {
		return errReadRequest
	}
	return nil
}

func validFetchArgument(line string) bool {
	for _, prefix := range []string{"have ", "shallow "} {
		if strings.HasPrefix(line, prefix) {
			return readOID(strings.TrimPrefix(line, prefix))
		}
	}
	switch line {
	case "done", "thin-pack", "no-progress", "include-tag", "ofs-delta", "deepen-relative", "wait-for-done":
		return true
	}
	for _, prefix := range []string{"deepen ", "deepen-since "} {
		if strings.HasPrefix(line, prefix) {
			n, err := strconv.ParseUint(strings.TrimPrefix(line, prefix), 10, 32)
			return err == nil && n > 0
		}
	}
	if strings.HasPrefix(line, "deepen-not ") {
		value := strings.TrimPrefix(line, "deepen-not ")
		return value != "" && !strings.ContainsAny(value, " \t")
	}
	// filter, want-ref and packfile-uris are NOT advertised by this path.
	return false
}

func (out *gitReadRequest) parseV0(p []gitPacket) error {
	out.phase = "fetch"
	if len(p) == 1 && p[0].kind == 0 {
		out.phase = "discover"
		return nil
	}
	wantsDone, ended := false, false
	for i, pkt := range p {
		if ended || pkt.kind == 1 {
			return errReadRequest
		}
		if pkt.kind == 0 {
			if !wantsDone {
				if len(out.wants) == 0 {
					return errReadRequest
				}
				wantsDone = true
			} else if i == len(p)-1 {
				ended = true
			} else {
				return errReadRequest
			}
			continue
		}
		line := pkt.line
		if strings.HasPrefix(line, "want ") && !wantsDone {
			fields := strings.Split(line, " ")
			if len(fields) < 2 || len(fields) > 2 && len(out.wants) != 0 {
				return errReadRequest
			}
			if err := out.addWant(fields[1]); err != nil {
				return err
			}
			continue // Native Git validates the first want's capabilities.
		}
		if wantsDone {
			if line == "done" && i == len(p)-1 {
				ended = true
			} else if !strings.HasPrefix(line, "have ") || !readOID(strings.TrimPrefix(line, "have ")) {
				return errReadRequest
			}
		} else if !strings.HasPrefix(line, "shallow ") && !strings.HasPrefix(line, "deepen") || !validFetchArgument(line) {
			return errReadRequest
		}
	}
	if !wantsDone || len(out.wants) == 0 || !ended && p[len(p)-1].kind != 0 {
		return errReadRequest
	}
	return nil
}
