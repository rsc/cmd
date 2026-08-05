/*
Mote connects to remote machine and runs commands,
especially cross-compiled Go tests.
It can connect using a variety of mechanisms:
SSH, Gomote, TCP over Tailscale, and authenticated direct TCP.

Usage:

	mote [-u path]... [@name] cmd [args...]
	mote alias [name [URL]]
	mote clean
	mote close [URL]
	mote go-setup
	mote login URL
	mote serve URL
	mote version

# Running Programs

The simplest mote command names a host and a command to run:

	% mote @ssh://kremvax hostname
	kremvax.uucp
	%

In the command, ssh://kremvax is a URL denoting the server to use.
The “Using SSH” section below explains how to set up kremvax.

A more interesting example is to upload and run a cross-compiled Go binary.
Assuming kremvax is an x86-64 Linux system:

	% GOOS=linux GOARCH=amd64 go build $(go env GOROOT)/test/helloworld.go
	% mote @ssh://kremvax ./helloworld
	hello world
	%

Mote treats the command name as a file to be uploaded when it contains a slash:
./helloworld is uploaded, but hostname is not.
The name resolves to a file the way running it locally would,
so on Windows “mote ./strings” uploads and runs ./strings.exe.

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
If there is no alias named $GOOS-$GOARCH already, mote adds one resolving to that system.
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

The script names no server, so mote uses the fallbacks listed above.
Cross-compiling on the command line sets $GOOS and $GOARCH in the
environment that “go test” passes to the script, selecting the alias for
the target system; using “go env -w” instead would leave them unset,
and mote would read them from the test binary.
Either way, the implied server is the $GOOS-$GOARCH alias.

Setting $MOTE overrides that choice for a single command, naming an alias
or a URL to use instead:

	% GOOS=linux GOARCH=amd64 go test strings
	PASS
	% MOTE=kremvax GOOS=linux GOARCH=amd64 go test strings
	PASS
	% MOTE=ssh://kremvax GOOS=linux GOARCH=amd64 go test strings
	PASS
	%

Setting $MOTE is useful when more than one server runs the same $GOOS-$GOARCH.

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

Using mote over SSH requires that the server be directly accessible.
Mote's builtin Tailscale library removes that requirement.

To serve mote over Tailscale:

	mote serve tail://servername

where servername is the desired server name.

To run a remote command:

	mote @tail://servername hostname

The first time a client or server uses Tailscale, it needs to connect to the tailnet.
Mote will prompt for a Tailscale auth key and then use that
auth key to register and obtain credentials.
It caches those credentials for reuse in future runs.

To create an auth key, open the “Keys” page of the Tailscale admin console
(https://login.tailscale.com/admin/settings/keys) and click “Generate auth key”.
Mote registers with the tag “tag:mote”, so before generating the key,
define tag:mote in the tailnet policy file's “tagOwners” section,
and then select tag:mote on the key generation form.
Tagged nodes have no expiring human identity attached,
and the tag makes it easy to write network policy rules for mote servers.

The server registers on the tailnet as mote-servername
and the client registers as mote-clientname,
where clientname is the first element of the local host name.
To set a different client name, run “mote login tail://clientname”.
A machine only ever registers one node: if it is already logged in
under some name, whether from “mote login” or “mote serve”,
mote uses that login instead of registering again.

Once the system is registered, either by a previous “mote serve”
or an explicit “mote login”, “mote serve tail://servername”
can be shortened to “mote serve tail:”.

Bringing up a Tailscale node takes a few seconds, and Tailscale does not
expect nodes to come and go frequently, so mote keeps the node running
in a background daemon, the same way ssh keeps a connection.
The first mote that needs the tailnet starts the daemon; later ones find it
and reuse it, so only the first command pays for the connection.
The daemon exits after 30 minutes with nothing to do.

Mote's tailscale client is built entirely into the mote binary
and does not reconfigure or otherwise affect the host networking stack.
Other programs on the machine will not use the Tailscale connection
managed by mote.

# Using Direct TCP

If the client can connect to the server, the simplest mechanism is direct TCP.
For direct TCP, the URL has the form tcp://host:port.
Client and server share a password, used to authenticate the connection
and then to establish an encrypted conversation.

Running “mote login URL” prompts for the password to use with a server
and saves it for later runs:

	% mote login tcp://kremlsun:6683
	password for tcp://kremlsun:6683:
	mote: wrote password for tcp://kremlsun:6683 to /home/rsc/.config/mote/password.txt
	%

Each machine saves the password under the URL that machine uses,
so the client and the server can name the same server differently.
The server logs in using the URL it serves, and then serves it:

	% mote login tcp://:6683
	password for tcp://:6683:
	mote: wrote password for tcp://:6683 to /home/rsc/.config/mote/password.txt
	% mote serve tcp://:6683
	mote: serving tcp://kremlsun:6683

The host can be omitted (tcp://:port) to mean listening on all interfaces.
The mote command will print a completed URL when it begins serving.

To run a remote command:

	% mote @tcp://kremlsun:6683 hostname
	kremlsun.arpa
	%

Each server has its own password: logging in to a second server adds
an entry instead of replacing the first.

# Using Gomotes

The Go project runs a custom remote execution facility known as gomotes,
which provide access to the various builders used for Go's own testing.
If the gomote command is found on the PATH, mote can use these servers.

To run a remote command:

	% mote @gomote://gotip-linux-amd64 hostname
	TODO
	%

(If you haven't used gomote recently, you may need to run “gomote login” first.)

The mote client installs and starts the mote server after creating the gomote.

If the gomote command is found on the PATH, mote creates GOOS-GOARCH aliases
backed by gomotes as needed. For example:

	% mote go-setup
	% mote alias linux-arm64
	mote: no alias for linux-arm64
	% GOOS=linux GOARCH=arm64 go test strings
	PASS
	% mote alias linux-arm64
	gomote://gotip-linux-arm64
	%

# Closing Servers

Each transport leaves the connection open for the next mote command,
timing out after 30 minutes.
To shut these down early, use “mote close URL”:

	% mote close ssh://kremvax
	% mote close tail:
	% mote close gomote://gotip-linux-amd64
	mote: destroyed gomote user-gotip-linux-amd64-0
	%

Running “mote close” with no URL closes everything: every shared ssh
connection, every local Tailscale daemon, and every gomote instance
that mote created.

	% mote close
	mote: closed shared ssh connection to kremvax
	mote: stopped tailscale daemon for mote-mac
	mote: destroyed gomote user-gotip-linux-amd64-0
	%

# Configuration

Mote stores its configuration in a mote subdirectory
of the user configuration directory. For example:

  - /home/rsc/.config/mote/ on Linux
  - /Users/rsc/Library/Application Support/mote on macOS
  - C:\Users\rsc\AppData\Local\mote\ on Windows

In that directory:

  - aliases.txt contains the alias definitions, one alias per line.
  - password.txt contains the passwords shared with tcp:// servers,
    as written by “mote login”: one line per server, holding the server
    URL and then the password, separated by a space.
  - tail-name/ is a directory that holds the login credentials for tail://name,
    along with the service socket, lock, and log of the daemon holding that node.

Both the Tailscale credentials and the tcp:// passwords are stored in plain text,
protected only by the file permissions of the configuration directory
and the files in it.

Setting $MOTECONFIG overrides the location of the configuration directory.

A mote server keeps a content-addressed cache of uploaded files in a
mote/cache subdirectory of the user cache directory
(for example, /home/rsc/.cache/mote/cache on Linux).
Setting $MOTECACHE overrides the location of the cache directory.
Each time a command finishes, the server deletes cached files that
have gone unused for more than three hours.
Running “mote clean” deletes the entire cache.
*/
package main
