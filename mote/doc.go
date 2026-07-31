/*
Mote runs commands on remote machines,
especially cross-compiled Go tests.
It can run commands using a variety of mechanisms:
SSH, Gomote, direct TCP, and TCP over Tailscale.

Usage:

	mote [-u path]... [@name] cmd [args...]
	mote alias [name [URL]]
	mote serve URL
	mote login URL
	mote go-setup
	mote version

# Running Programs

The simplest mote command names a host and a command to run:

	% mote @ssh://kremvax hostname
	kremvax.uucp
	%

In the command, ssh://kremvax is a URL denoting the server to use.
The “Using SSH” section below explains how to set up kremvax.

A more interesting example is to upload and run a cross-compiled Go binary.
Assuming kremvax is now an x86-64 Linux system:

	% GOOS=linux GOARCH=amd64 go build $(go env GOROOT)/test/helloworld.go
	% mote @ssh://kremvax ./helloworld
	hello world
	%

Mote treats the command name as a file to be uploaded when it contains a slash:
./helloworld is uploaded, but hostname is not.

# Uploading Additional Files

The command runs in a remote temporary directory that includes the local directory name.
For example, consider:

	% cd /home/rsc/src/myprog
	% mote @ssh://kremvax ../testprog

In this case, the remote command might run in /tmp/mote-123/home/rsc/src/myprog,
with testprog uploaded to /tmp/mote-123/home/rsc/src/testprog.

The repeatable -u flag specifies additional files or directories to upload into
the remote temporary directory tree. For example if a test binary required testdata:

	% cd /home/rsc/src/mypkg
	% GOOS=linux GOARCH=amd64 go test -c
	% mote -u ./testdata @ssh://kremvax ./mypkg.test
	PASS
	%

The -t flag uploads testdata, ../testdata, ../../testdata, and so on,
if they exist, up to the current Go module root:

	% mote -t @ssh://kremvax ./mypkg.test

# Server Aliases and Server Selection

The “mote alias” command defines an alias for a URL:

	% mote alias kremvax ssh://kremvax
	% mote @kremvax ./mypkg.test
	PASS
	%

Running “mote alias name” expands the alias:

	% mote alias kremvax
	ssh://kremvax
	%

Running “mote alias” without any arguments lists all the known aliases:

	% mote alias
	kremvax     ssh://kremvax
	linux-amd64 ssh://kremvax
	%

Each time a mote command runs, it discovers the GOOS and GOARCH of the remote system.
If there is no alias for that $GOOS-$GOARCH already, mote defines one automatically.
In this case, mote has defined a linux-amd64 alias.

If the “@server” is omitted, mote tries three fallbacks, in order:

  - $MOTE, if set.
  - $GOOS-$GOARCH, if those variables are set.
  - $GOOS-$GOARCH, where those variables are derived from the binary to be uploaded, if possible.

Demonstrating all three:

	% MOTE=kremvax mote ./mypkg.test
	% GOOS=linux GOARCH=amd64 mote ./mypkg.test
	% mote ./mypkg.test

# Go Run and Go Test Integration

The Go toolchain handles “go run” and “go test” of cross-compiled binaries by
looking for a runner named go_$GOOS_$GOARCH_exec in the shell path.
To create runner scripts for all GOOS-GOARCH combinations that don't already have one:

	% mote go-setup
	installed 10 hooks in /home/rsc/bin
	% mote go-setup
	no hooks left to install
	%

The runner script is “mote -t” applied to the arguments:

	% cat /home/rsc/bin/go_linux_amd64_exec
	#!/bin/sh
	exec mote -t "$@"
	%

# Using SSH

To use mote over SSH, compile and install mote on both client and server, and check that it is available on the server PATH:

	% go install rsc.io/cmd/mote@latest
	% ssh kremvax 'go install rsc.io/cmd/mote@latest && mote version'
	% mote @ssh://kremvax hostname
	kremvax.uucp
	%

Mote runs ssh with persistence enabled with a 30-minute timeout,
so that repeated uses of mote can share a long-running ssh connection.
Specifically, it runs:

	ssh \
		-o 'ControlMaster auto' \
		-o 'ControlPath ~/.ssh/sockets/mote-%r@%h-%p' \
		-o 'ControlPersist 1800' \
		kremvax \
		mote serve -

# Using Tailscale

Using mote over SSH requires that the server be directly accessible. Using Tailscale removes that requirement.

To serve mote over Tailscale:

	mote serve tail://servername

where servername is the desired server name.

To run a remote command:

	mote @tail://servername hostname

The first time a client or server uses Tailscale, it needs to connect to the tailnet.
Mote will prompt for a Tailscale auth key and then use that
auth key to register and obtain credentials.
It caches those credentials for reuse in future runs.

The server registers on the tailnet as mote-servername
and the client registers as mote-clientname,
where clientname is the first element of the local host name.
To set a different client name, run “mote login tail://clientname”.

Once the system is registered, either by a previous “mote serve”
or an explicit “mote login”, “mote serve tail://servername”
can be shortened to “mote serve tail:”.

# Using Direct TCP

If the client can connect to the server, the simplest mechanism is direct TCP.
For direct TCP, the URL has the form tcp://host:port/password, where password
is a password shared between client and server,
used to authenticate the connection and then to establish an encrypted conversation.

To serve direct TCP:

	% mote serve tcp://host:port/password
	mote: serving tcp://host:port/password

The host can be omitted (tcp://:port/password) to mean listening on all interfaces.
The mote command will print a completed URL when it begins serving.

To run a remote command:

	% mote @tcp://host:port/password hostname
	moskvax
	%

# Using Gomotes

The Go project runs a different remote execution facility known as gomotes,
which provide access to the various builders used for Go's own testing.
If the gomote command is found on the PATH, mote can use these servers.

To run a remote command:

	% mote @gomote://gotip-linux-amd64 hostname
	TODO
	%

The mote client installs and starts the mote server after creating the gomote.

If the gomote command is found on the PATH, mote creates GOOS-GOARCH aliases
backed by gomotes the first time they are needed. For example:

	% mote go-setup
	% GOOS=freebsd GOARCH=amd64 go test strings
	PASS
	%

# Configuration

Mote stores its configuration in a mote subdirectory
of the user configuration directory. For example:

  - /home/rsc/.config/mote/ on Linux
  - /Users/rsc/Library/Application Support/mote on macOS
  - C:\Users\rsc\AppData\Local\mote\ on Windows

In that directory:

  - aliases.txt contains the alias definitions, one alias per line.
  - tail-name/ is a directory that holds the login credentials for tail://name.
*/
package main

/*
PROTOCOL

However the connection is established,
the protocol between client and server starts with up to 64 kB of text from the server that is ignored,
which may be login banners or other output.
Eventually the server should print

	mote server hello \x00\x01\xfe\xff\n

on a line by itself.
If the server sends more than 64 kB before sending that text,
or if the server sends other text and then stops printing for more than 60 seconds,
then the client should interpret that text as an error message and report it.

If the client sees the mote 1.0 server hello but the wrong bytes before end of line,
it should report a non-binary-safe connection and exit.

The client sends back

	mote client hello \xff\xfe\x01\x00\n

When using a direct TCP connection, the next step is authentication
using the shared secret and CPACE and no authenticated data.
The server role is Initiator, the client role is Responder.
After the CPACE handshake, there is a verification round
exchanging Macs. At this point a shared key is established.
Then the protocol uses Noise NNpsk0 (25519, ChaChaPoly, SHA256)
to establish an encrypted channel and the main protocol continues.
The Noise pre-shared key is derived from the CPace intermediate
session key by HKDF-SHA256 with the info string "mote noise psk".
In the Noise handshake, the client is the initiator.

The CPace messages, verification Macs, and Noise handshake messages
travel in the standard packet framing described below, with a zero-length
JSON section and the message bytes as the binary data. After the
handshake, each Noise transport message is framed by a 16-bit
big-endian ciphertext length, and the standard packet framing runs
on top of the resulting encrypted stream.

The protocol consists of a request respponse protocol in which
the client sends a request and then the server sends one or more responses.
Each packet is framed by a pair of 32-bit big-endian lengths.
The first specifies a number of JSON bytes for the request metadata
(always a JSON object),
and the second specifies a number of raw data bytes that follow the JSON.
The JSON Go structs are:

	type Request struct {
		Type string
		Error string `json:",omitzero"`
		Files []*File `json:",omitzero"`
		Cmd string `json:",omitzero"`
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
		Stderr bool
		ExitCode int `json:",omitzero"`
		Status string `json:",omitzero"`
		GOOS string `json:",omitzero"`
		GOARCH string `json:",omitzero"`
	}

File paths and hashes: Path is the file's absolute path on the client
in slash-separated form; the server strips any leading volume name and
re-roots the path in its temporary directory, creating each file with
mode 0755. Hash is the lowercase hex SHA-256 of the file content.

The server reports its GOOS and GOARCH in the first response of each
session (whether Need, Start, or Exit), for the client's automatic
$GOOS-$GOARCH aliases.

Any response may set Error, which the client reports as a fatal error.
If the command cannot be started, the server sends Type "Exit" with
Error set and no Start.

The client starts by sending a request with Type "Run" and specifying
the list of files to be placed, the directory where the command is run,
and the full arguments for os/exec; Args[0] is the command name again.
Env is additional environment variables to set beyond the server's defaults.

If server does not have any files cached, it replies with "Type": "Need",
with Need set to a list of missing hashes. It knows the size of each
missing file, since that was included in the Run request.
The client responds with "Type": "Upload" with no other fields.
The binary data count must be the sum of the requested files' sizes.
The client uploads the files in the order specified, concatenated
in one long stream.
The server reads them individually, saving each one to the content-addressed cache.

When the upload is finished, or if the server never replied with Type Need,
the server moves on to running the command.
If it starts the command successfully, it sends a message of Type "Start"
with no other data.

After receiving Start, the protocol becomes concurrent.
At any point, the client can send Type "Kill" to kill the process.
The server must kill the process and then proceed to the eventual Exit message.

The server sends zero or more Type "Output" with the binary data being a chunk of output.
If Stderr is true, the data is standard error. Otherwise it is standard output.
The two separate streams do mean that sometimes stdout and stderr
can be reordered relative to each other.

After sending all the output, the server sends Type "Exit", with the ExitCode and Status fields set.
Then it hangs up.

After receiving the server Exit, the client hangs up.

For connection establishment, direct TCP is obviously direct TCP.
Tailscale uses TCP port 6683 (MOTE).

Gomote runs a put to upload the binary and then runs 'gomote ssh /path/to/mote serve -'.
Gomote is the one I am concerned about not being binary safe,
especially gomote to Windows.
*/

/*
IMPLEMENTATION

The command should follow the same general conventions as the other commands
in this tree as far as usage messages, structure, log prints.
Tailscale should use tailscale.com/tsnet.
See /Users/rsc/src/rsc.io/tmp/tschat for an example that tested that tsnet worked.

There is a cpace implementation in internal/cpace.

TESTING

Most testing can be done using testing/synctest and a fake network connection,
checking both with and without a password.

Mocking the SSH and Tailscale connections will be more difficult.
A mock ssh can be made by running the test binary with os.Args[0] set to "ssh".

A mock gomote command will also be needed,
probably running the test binary with os.Args[0] set to "gomote".
Since we only use gomote put and gomote ssh that should not be too hard to mock.
*/
