// Copyright 2026 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

package main

import (
	"bytes"
	"io"
	"math/rand/v2"
	"os"
	"strings"
	"testing"
	"time"
)

func TestHexRoundTrip(t *testing.T) {
	// Data with every byte value, long enough to span many encoded
	// lines, read back in sizes that do not divide the line length.
	data := make([]byte, 5000)
	for i := range data {
		data[i] = byte(i)
	}
	var buf bytes.Buffer
	w := &hexWriter{w: &buf}
	for rest := data; len(rest) > 0; {
		n := min(len(rest), 1+rand.IntN(300))
		if _, err := w.Write(rest[:n]); err != nil {
			t.Fatalf("Write: %v", err)
		}
		rest = rest[n:]
	}
	for _, line := range strings.Split(strings.TrimSuffix(buf.String(), "\n"), "\n") {
		if len(line) > 2*hexLineBytes {
			t.Fatalf("encoded line of %d bytes, want at most %d", len(line), 2*hexLineBytes)
		}
	}

	got, err := io.ReadAll(&hexReader{r: bytes.NewReader(buf.Bytes())})
	if err != nil {
		t.Fatalf("ReadAll: %v", err)
	}
	if !bytes.Equal(got, data) {
		t.Fatalf("decoded %d bytes, want the %d written", len(got), len(data))
	}
}

func TestHexReader(t *testing.T) {
	tests := []struct {
		name string
		in   string
		want string
		err  string
	}{
		{name: "empty"},
		{name: "plain", in: "6d6f7465", want: "mote"},
		{name: "uppercase", in: "6D6F7465", want: "mote"},
		// The newlines the encoder inserts, and the carriage returns a
		// terminal inserts alongside them, carry no data.
		{name: "newlines", in: "6d6f\n7465\n", want: "mote"},
		{name: "crlf", in: "6d6f\r\n7465\r\n", want: "mote"},
		{name: "split", in: "6\nd\n6\nf\n", want: "mo"},
		// A byte that is neither is the far end talking out of band.
		{name: "text", in: "6d6f\nmote: server: no such file\n", want: "mo",
			err: `unencoded data on hex connection: "mote: server: no such file"`},
		// A hex digit with no partner is a truncated stream, not an error:
		// the reader reports the bytes it has and waits for the rest.
		{name: "odd", in: "6d6f7", want: "mo"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			// One byte at a time from the underlying reader, so that
			// pairs and lines split across reads.
			got, err := io.ReadAll(&hexReader{r: iotest1Byte{strings.NewReader(tt.in)}})
			if string(got) != tt.want {
				t.Errorf("read %q, want %q", got, tt.want)
			}
			switch {
			case tt.err == "" && err != nil:
				t.Errorf("read error %v, want success", err)
			case tt.err != "" && (err == nil || !strings.Contains(err.Error(), tt.err)):
				t.Errorf("read error %v, want %s", err, tt.err)
			}
		})
	}
}

// An iotest1Byte reads one byte at a time from r. It accepts read
// deadlines, so that the decoder collects a whole unencoded message.
type iotest1Byte struct{ r io.Reader }

func (r iotest1Byte) Read(p []byte) (int, error) {
	if len(p) == 0 {
		return 0, nil
	}
	return r.r.Read(p[:1])
}

func (r iotest1Byte) SetReadDeadline(time.Time) error { return nil }

func TestScanHexHandshake(t *testing.T) {
	tests := []struct {
		name string
		in   string
		ok   bool
	}{
		{"plain", hexHandshake, true},
		{"crlf", "mote hex handshake\r\n", true},
		// What a shell on a terminal really sends: its own output, the
		// echo of the command line, and escape sequences that land on
		// the same line as the handshake.
		{"terminal", "$ ssh gomote\r\nuser@host:~$ exec " + moteServeHex + "\r\n" +
			"\x1b[?2004l\rmote hex handshake\r\n", true},
		{"missing", "user@host:~$ ./mote: not found\r\n", false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := scanHexHandshake(strings.NewReader(tt.in))
			if (err == nil) != tt.ok {
				t.Errorf("scanHexHandshake: %v, want ok=%v", err, tt.ok)
			}
			if err != nil && !strings.Contains(err.Error(), "hex handshake") {
				t.Errorf("scanHexHandshake: %v, want the error to name what was missing", err)
			}
		})
	}
}

// TestHexSession runs a whole session over a hex-encoded connection
// that mangles newlines the way a terminal does.
func TestHexSession(t *testing.T) {
	setupDirs(t)
	cconn, sconn := osPipePair(t)
	done := make(chan error, 1)
	go func() {
		done <- serve(newHexConn(crlfConn{sconn}), "", nil)
		sconn.Close()
	}()
	conn, err := clientConn(newHexConn(cconn), "")
	if err != nil {
		t.Fatalf("clientConn: %v", err)
	}
	runConn(t, conn, []string{"echo", "in hex"}, "in hex\n")
	cconn.Close()
	<-done
}

// A crlfConn is a connection that turns every newline written to it
// into a carriage return and a newline, as a terminal does.
type crlfConn struct{ io.ReadWriteCloser }

func (c crlfConn) Write(p []byte) (int, error) {
	if _, err := c.ReadWriteCloser.Write(bytes.ReplaceAll(p, []byte("\n"), []byte("\r\n"))); err != nil {
		return 0, err
	}
	return len(p), nil
}

// osPipePair returns the two ends of a connection made from a pair of
// operating system pipes. Unlike net.Pipe, the pipes hold what has
// been written until it is read, which is what a real transport does
// and what a protocol with trailing bytes (the newlines that hex
// encoding adds) needs to avoid deadlocking.
func osPipePair(t *testing.T) (a, b io.ReadWriteCloser) {
	t.Helper()
	ar, bw, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	br, aw, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		ar.Close()
		aw.Close()
		br.Close()
		bw.Close()
	})
	return &filePair{ar, aw}, &filePair{br, bw}
}

// A filePair is the reading and writing halves of a connection.
type filePair struct{ r, w *os.File }

func (p *filePair) Read(b []byte) (int, error)  { return p.r.Read(b) }
func (p *filePair) Write(b []byte) (int, error) { return p.w.Write(b) }
func (p *filePair) Close() error                { p.r.Close(); return p.w.Close() }
