# The Mote Protocol

This file describes the protocol that a mote client and a mote server
speak over an established connection. The connection may be any
byte stream: the standard input and output of an ssh or gomote
subprocess, a direct TCP connection, or a TCP connection over
Tailscale (which uses port 6683, MOTE).

The protocol is a sequence of packets. Each packet is framed by a pair
of 32-bit big-endian lengths: the first counts the bytes of a JSON
section (always a JSON object when non-empty), and the second counts
the bytes of a raw binary section that follows the JSON. Either
section may be empty. Client packets are requests; server packets are
responses. The JSON sections correspond to these Go structs:

	type Request struct {
		Type string
		Error string `json:",omitzero"`
		Files []*File `json:",omitzero"`
		Args []string `json:",omitzero"`
		Dir string `json:",omitzero"`
		Env []string `json:",omitzero"`
	}

	type File struct {
		Path string
		Hash string
		Size int64
	}

	type Response struct {
		Type string
		Error string `json:",omitempty"`
		Need []string `json:",omitempty"`
		Stderr bool `json:",omitzero"`
		ExitCode int `json:",omitzero"`
		Status string `json:",omitzero"`
		GOOS string `json:",omitzero"`
		GOARCH string `json:",omitzero"`
	}

The request types are Setup, Upload, Start, and Kill.
The response types are Info, Need, Ready, Output, and Exit.

Any response may set Error, which the client reports as a fatal error.
A server that cannot continue (a failed upload, a command that cannot
be started) sends a final response of type Exit with Error set.

## Handshake

The conversation begins with up to 64 kB of text from the server that
the client ignores; this may be ssh login banners or other output from
the connection machinery. Eventually the server sends

	mote server hello \x00\x01\xfe\xff\n

on a line by itself. If the server sends more than 64 kB without that
line, sends other text and then nothing for more than 60 seconds, or
hangs up, the client reports the accumulated text as an error message.

If the client sees the text “mote server hello ” but the wrong bytes
before the end of the line (for example, a trailing \r\n), the
connection is corrupting binary data, and the client reports that the
connection is not binary safe.

The client answers with its own hello:

	mote client hello \xff\xfe\x01\x00\n

The server applies the same binary-safety check to that line.

## Authentication

A direct TCP connection (tcp://host:port/password) is authenticated
and encrypted using the URL's password; the other transports are
already authenticated and encrypted by ssh, gomote, or Tailscale, and
skip this step.

First, a CPace handshake (X25519, SHA-512, channel identifier
“rsc.io/cmd/mote tcp”, no additional data) proves that both sides hold
the same password without revealing it. The server is the CPace
initiator and the client the responder. After the two CPace messages,
each side sends its confirmation tag and verifies the tag it receives;
a bad tag means the passwords do not match, and the connection ends.

The CPace intermediate session key is then run through HKDF-SHA256
with the info string “mote noise psk” to produce a 32-byte pre-shared
key for a Noise NNpsk0 handshake (25519, ChaChaPoly, SHA256) in which
the client is the initiator. The Noise handshake establishes the
encrypted channel.

The CPace messages, confirmation tags, and Noise handshake messages
all travel in the standard packet framing, with an empty JSON section
and the message bytes as the binary section. After the handshake, each
Noise transport message (at most 65535 bytes of ciphertext, including
the 16-byte tag) is framed by a 16-bit big-endian ciphertext length,
and the packet framing runs on top of the resulting encrypted stream.

## Setup

The server speaks first, sending a response of type Info with its
GOOS and GOARCH set. The client uses these to define $GOOS-$GOARCH
aliases automatically.

The client then sends a request of type Setup describing the command
to run: Files lists the files to be placed on the server, Dir is the
client's working directory, Args is the full argument list for os/exec
(Args[0] is the command name), and Env is any additional environment
variables beyond the server's defaults.

For each file, Path is the file's absolute path on the client in
slash-separated form, Hash is the lowercase hex SHA-256 of the file
content, and Size is its length in bytes. The server strips any
leading volume name (like C:) from the path and re-roots it in a fresh
temporary directory, creating each file with mode 0755. The server
maps Dir the same way, so a command name like ./prog or ../testprog is
resolved relative to the re-rooted directory. Paths containing ..
elements are rejected.

If any hashes are missing from the server's content-addressed cache,
the server replies with a response of type Need listing them. The
client answers with a request of type Upload whose binary section is
the contents of the needed files, in the order requested, concatenated;
its length must be the sum of those files' sizes. The server saves
each file to its cache, verifying the hashes.

Once every file is cached and the temporary tree is built, the server
sends a response of type Ready. The command is not yet running.

## Execution

The client sends a request of type Start, and the server starts the
command. After Start, the protocol becomes concurrent: at any point
the client can send a request of type Kill, and the server kills the
command (and its process group) and proceeds to the eventual Exit. The
server also kills the command if the client hangs up.

As the command runs, the server sends responses of type Output whose
binary sections are chunks of command output, with Stderr reporting
whether a chunk is standard error rather than standard output. The two
streams are forwarded separately, so output can sometimes be reordered
relative to the interleaving on the server.

When the command finishes and all output has been sent, the server
sends a response of type Exit with ExitCode and Status (a
human-readable description of how the command exited) set, and then
hangs up. ExitCode is negative if the command was killed by a signal.
After receiving Exit, the client hangs up.
