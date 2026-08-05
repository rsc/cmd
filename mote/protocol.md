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
		Addr string `json:",omitzero"`
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
The Tailscale daemon, described at the end of this file, adds the
request types Dial and Serve and the response types Connected,
Serving, and Log.

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

## Hex Encoding

Some connections cannot carry arbitrary bytes. The gomote transport may
have to run the mote server in a shell on a terminal (see the “Using
Gomotes” section of doc.go), and the terminals along the way rewrite
newlines, echo back what they read, and act on control characters
instead of passing them along.

A server started as `mote serve -hex-` runs the whole conversation
above in hex. That spelling is a URL, not a flag — only mote itself has
any reason to ask for hex, so it is left out of the documented usage.
Before any encoding, the server writes

	mote hex handshake\n

which the client looks for the way it looks for the server hello: after
whatever the shell has already printed, including the echo of the
command line that started the server. A terminal may deliver the line
as `...\r\n` and may leave escape sequences ahead of it on the same
line, so the client matches the end of a line rather than a whole line.

After that line, every byte in both directions is two hex digits.
The encoder writes a newline after every 64 bytes of data, keeping the
lines short enough for a terminal that is still buffering input by the
line; decoders ignore carriage returns and newlines wherever they
appear, including between the two digits of a byte. Anything else in
the stream is not protocol data but a message from the far end, and
decoding fails reporting that text.

The encoding leaves only hex digits and newlines on the wire, so no
terminal has anything to act on — but a terminal that echoes would
still feed the client its own bytes back. Both ends put their terminal
in raw mode to stop that: mote before it starts the transport
subprocess, and the hex server on the terminal it finds on its
standard input.

## Authentication

A direct TCP connection (tcp://host:port) is authenticated and
encrypted using a password shared by client and server, which each
keeps in password.txt in its configuration directory, keyed by the URL
it uses for the server; the other transports are already authenticated
and encrypted by ssh, gomote, or Tailscale, and skip this step.

First, a CPace handshake (X25519, SHA-512, channel identifier
“rsc.io/cmd/mote tcp”, no additional data) proves that both sides hold
the same password without revealing it. The server is the CPace
initiator and the client the responder. After the two CPace messages,
each side sends its confirmation tag and verifies the tag it receives;
a bad tag means the passwords do not match, and the connection ends.

The client sends its confirmation tag first, and the server sends its
own only after verifying the client's. Either order allows just one
password guess per connection, but this one keeps the server, which
listens on an open port, from confirming a guess to a client that has
not proved anything: a client that guessed wrong sees only a hangup.

The CPace intermediate session key is then run through HKDF-SHA256
with the info string “mote noise psk” to produce a 32-byte pre-shared
key for a Noise NNpsk0 handshake (25519, ChaChaPoly, SHA256) in which
the client is the initiator. The Noise handshake establishes the
encrypted channel.

The CPace messages, confirmation tags, and Noise handshake messages
all travel in the standard packet framing, with an empty JSON section
and the message bytes as the binary section. Because these packets
arrive before the peer has been authenticated, a handshake packet that
declares a JSON section or more than 1024 bytes of data is rejected on
the strength of its header, without allocating a buffer for it. A peer
that has not finished the handshake within 60 seconds is disconnected.

After the handshake, each Noise transport message (at most 65535 bytes
of ciphertext, including the 16-byte tag) is framed by a 16-bit
big-endian ciphertext length. That length is the message's associated
data, so altering it in flight fails decryption instead of silently
desynchronizing the framing. The packet framing then runs on top of
the resulting encrypted stream.

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
maps Dir the same way. Paths containing .. elements are rejected.

Args[0] runs from that tree when it names one of the uploaded files:
an absolute path names one directly (“go test” runs its test binaries
by absolute path), and a relative path containing a slash, like ./prog
or ../testprog, names one relative to Dir. A name with no slash at all
is not an uploaded file and is looked up on the server's own PATH.

A Windows server runs the command's file under a name Windows will
run: if the file is a Windows executable (not a library) whose name
does not already end in .exe, the server adds that suffix, since
Windows decides what a file is by its name and a binary
cross-compiled for Windows often arrives without one. Only the
command's own file is renamed; the rest keep the names the test
expects to find, testdata included.

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

## The Tailscale Daemon

Bringing a Tailscale node up takes a few seconds, so mote does not do
it once per command. Instead one background daemon holds the node and
the other mote commands on the machine — clients and the server alike
— reach it over a unix socket named `service` in the node's
configuration directory. The daemon exits after 30 minutes with no
connections. This is a second use of the packet framing above, on a
different connection: between a mote and its local daemon, not between
a client and a remote server.

A client that wants to reach a server sends a request of type Dial
with Addr set to the tailnet address (`mote-name:6683`). The daemon
answers with a response of type Connected, or of type Error with Error
set if it cannot reach the address. After Connected the packet framing
stops on that connection: the daemon copies raw bytes in both
directions between the client and the tailnet, and the client speaks
the protocol above through it to the remote server.

A request of type Stop (sent by “mote close”) asks the daemon to shut
down. The daemon answers with a response of type Stopping and then
exits, cutting off any other connected clients.

A server sends a request of type Serve, with Env set to the
environment its commands should run with. The daemon starts listening
on the tailnet and answers with a response of type Serving. It then
runs the sessions itself, as the server would, and sends its log
output — Tailscale's messages and any session errors — to the mote
server as responses of type Log whose binary sections are the text to
print. Only one server may be registered at a time; a second Serve is
refused with Error set. When the mote server hangs up, the daemon
stops listening; sessions already running are left to finish.

