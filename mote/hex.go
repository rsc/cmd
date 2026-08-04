// Copyright 2026 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

package main

import (
	"bytes"
	"encoding/hex"
	"fmt"
	"io"
	"strings"
	"time"
)

// Hex encoding, for connections that are not binary safe.
//
// The gomote transport can only reach some servers by running the mote
// server in a shell on a terminal (see gomoteShell). A terminal is not
// a byte-transparent channel: it rewrites newlines, echoes what it
// reads back at the writer, and acts on control characters instead of
// passing them along. Encoding the protocol in hex leaves nothing on
// the wire but hex digits and newlines, which every terminal carries
// unchanged, and the decoder ignores the newlines.
//
// The server enters hex mode when started as "mote serve -hex-". It
// writes hexHandshake unencoded and encodes everything after it; the
// client skips whatever the shell printed first, finds that line, and
// encodes in turn. The protocol above knows nothing about any of this.
// See protocol.md.

// hexHandshake is the line marking the start of hex encoding.
// A terminal may deliver it as "...\r\n" and may print escape
// sequences ahead of it on the same line, so the client matches the
// end of a line instead of a whole line.
const hexHandshake = "mote hex handshake\n"

// hexLineBytes is how many bytes of data one encoded line holds.
// The lines are short because a terminal that is still in canonical
// mode buffers input by the line and discards a line too long to fit.
const hexLineBytes = 64

// A hexConn is a connection carrying hex-encoded protocol bytes.
// It is an io.ReadWriteCloser, so the whole protocol runs on top of it
// unchanged, the way it runs on top of the encrypted stream.
type hexConn struct {
	rwc io.ReadWriteCloser
	r   hexReader
	w   hexWriter
}

func newHexConn(rwc io.ReadWriteCloser) *hexConn {
	return &hexConn{rwc: rwc, r: hexReader{r: rwc}, w: hexWriter{w: rwc}}
}

func (h *hexConn) Read(p []byte) (int, error)  { return h.r.Read(p) }
func (h *hexConn) Write(p []byte) (int, error) { return h.w.Write(p) }
func (h *hexConn) Close() error                { return h.rwc.Close() }

// SetReadDeadline sets a read deadline on the underlying connection,
// for the handshake stall timeout.
func (h *hexConn) SetReadDeadline(t time.Time) error {
	if d, ok := h.rwc.(deadlineReader); ok {
		return d.SetReadDeadline(t)
	}
	return errUnsupported
}

// abort tears down the underlying connection, so that a hex-encoded
// transport reports the same diagnostics as a plain one.
func (h *hexConn) abort(err error) error { return abortConn(h.rwc, err) }

// A hexWriter encodes the bytes written to it as lines of hex digits.
// Each Write becomes a single write to the underlying connection, so
// that the packets of the protocol above still arrive in one piece.
type hexWriter struct {
	w   io.Writer
	buf []byte
}

func (h *hexWriter) Write(p []byte) (int, error) {
	h.buf = h.buf[:0]
	for rest := p; len(rest) > 0; {
		line := rest
		if len(line) > hexLineBytes {
			line = line[:hexLineBytes]
		}
		rest = rest[len(line):]
		h.buf = hex.AppendEncode(h.buf, line)
		h.buf = append(h.buf, '\n')
	}
	if _, err := h.w.Write(h.buf); err != nil {
		return 0, err
	}
	return len(p), nil
}

// A hexReader decodes the hex digits read from a connection, ignoring
// the newlines that separate them. Anything else is an error: at this
// point in the conversation the only writer on the other end is the
// mote server, so unencoded text is a message about something that
// went wrong, and reporting it is more useful than dropping it.
type hexReader struct {
	r   io.Reader
	buf []byte // raw bytes, reused between reads
	hi  byte   // first digit of a pair split across reads
	odd bool   // hi is waiting for its second digit
	err error  // decoding failed; the connection is not recoverable
}

func (h *hexReader) Read(p []byte) (int, error) {
	if h.err != nil {
		return 0, h.err
	}
	if len(p) == 0 {
		return 0, nil
	}
	// Two digits per byte: reading that many keeps the decoded bytes
	// within p, whether or not a digit is left over from last time.
	if cap(h.buf) < 2*len(p) {
		h.buf = make([]byte, 2*len(p))
	}
	for {
		nr, err := h.r.Read(h.buf[:2*len(p)])
		n := 0
		for i, c := range h.buf[:nr] {
			v, ok := unhex(c)
			if !ok {
				if c == '\n' || c == '\r' {
					continue
				}
				h.err = fmt.Errorf("unencoded data on hex connection: %s", h.text(h.buf[i:nr]))
				return n, h.err
			}
			if !h.odd {
				h.hi, h.odd = v, true
				continue
			}
			p[n] = h.hi<<4 | v
			n++
			h.odd = false
		}
		if n > 0 || err != nil {
			return n, err
		}
		// Nothing but newlines; wait for more.
	}
}

// unhex returns the value of the hex digit c.
func unhex(c byte) (byte, bool) {
	switch {
	case '0' <= c && c <= '9':
		return c - '0', true
	case 'a' <= c && c <= 'f':
		return c - 'a' + 10, true
	case 'A' <= c && c <= 'F':
		return c - 'A' + 10, true
	}
	return 0, false
}

// text formats the unencoded bytes b for an error message, first
// waiting briefly for the rest of the line if it is still on its way:
// the message is the far end explaining why the session is failing,
// and half of it explains half as much.
func (h *hexReader) text(b []byte) string {
	msg := bytes.Clone(b)
	if d, ok := h.r.(deadlineReader); ok && d.SetReadDeadline(time.Now().Add(textTimeout)) == nil {
		var buf [512]byte
		for !bytes.ContainsAny(msg, "\r\n") && len(msg) < cap(buf) {
			n, err := h.r.Read(buf[:])
			msg = append(msg, buf[:n]...)
			if err != nil {
				break
			}
		}
		d.SetReadDeadline(time.Time{})
	}
	s := strings.TrimSpace(string(msg))
	if i := strings.IndexAny(s, "\r\n"); i >= 0 {
		s = s[:i]
	}
	if len(s) > 256 {
		s = s[:256] + "..."
	}
	return fmt.Sprintf("%q", s)
}

// textTimeout is how long the decoder waits for the rest of an
// unencoded message before reporting the part that has arrived.
const textTimeout = 1 * time.Second

// scanHexHandshake reads the connection up to and including the
// server's hex handshake line, ignoring the shell output ahead of it
// the same way the client ignores an ssh login banner.
func scanHexHandshake(r io.Reader) error {
	want := strings.TrimSuffix(hexHandshake, "\n")
	return scanHello(r, "hex handshake", func(line string) (bool, error) {
		return strings.HasSuffix(strings.TrimRight(line, "\r\n"), want), nil
	})
}
