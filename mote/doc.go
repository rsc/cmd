/*
Mote runs commands on remote machines,
especially cross-compiled Go tests.
It can run commands using a variety of mechanisms:
SSH, Gomote, direct TCP, and TCP over Tailscale.

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

The script names no server, so mote uses the fallbacks listed above.
Cross-compiling on the command line sets $GOOS and $GOARCH in the
environment that “go test” passes to the script, selecting the alias for
the target system; using “go env -w” instead leaves them unset, and mote
reads them from the test binary. Either way the server is the
$GOOS-$GOARCH alias.

Setting $MOTE overrides that choice for a single command, naming an alias
or a URL to use instead:

	% GOOS=linux GOARCH=amd64 go test strings
	PASS
	% MOTE=kremvax GOOS=linux GOARCH=amd64 go test strings
	PASS
	% MOTE=ssh://kremvax GOOS=linux GOARCH=amd64 go test strings
	PASS
	%

That is useful when more than one server runs the same $GOOS-$GOARCH,
or to send one test run to a particular machine without redefining
the alias.

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

To create an auth key, open the “Keys” page of the Tailscale admin console
(https://login.tailscale.com/admin/settings/keys) and click “Generate auth key”.
Mote registers with the tag “tag:mote”, so before generating the key,
define tag:mote in the tailnet policy file's “tagOwners” section,
and then select tag:mote on the key generation form.
Tagged nodes have no expiring human identity attached,
and the tag makes it easy to write policy rules for mote servers.

The server registers on the tailnet as mote-servername
and the client registers as mote-clientname,
where clientname is the first element of the local host name.
To set a different client name, run “mote login tail://clientname”.
A machine only ever registers one node: if it is already logged in
under some name, whether from “mote login” or “mote serve”,
mote uses that login instead of registering again as the host name.

Once the system is registered, either by a previous “mote serve”
or an explicit “mote login”, “mote serve tail://servername”
can be shortened to “mote serve tail:”.

Bringing a Tailscale node up takes a few seconds, and Tailscale does not
expect nodes to come and go once per command, so mote keeps the node running
in a background daemon, the way ssh keeps a connection with ControlMaster.
The first mote that needs the tailnet starts the daemon; later ones find it
and reuse it, so only the first command pays for the connection.
The daemon exits after 30 minutes with nothing to do.

Because the daemon holds the node, a mote client and a mote server on the
same machine share it, which is not possible when each command brings up a
node of its own.

The daemon keeps its socket and its log in the node's configuration
directory, tail-name. If a mote command reports that the daemon would not
start, tail-name/log has the reason.

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

# Closing Servers

Each transport leaves something running so that the next mote command
is fast: ssh keeps a shared connection open for 30 minutes, Tailscale
keeps the local node's daemon running for 30 minutes after its last
use, and a gomote instance lives until its lease expires. All of these
go away on their own, but “mote close URL” shuts them down early:

	% mote close ssh://kremvax
	% mote close tail:
	% mote close gomote://gotip-linux-amd64
	mote: destroyed gomote user-gotip-linux-amd64-0
	%

For ssh, close stops the shared connection to the named host. For
Tailscale, close stops the local daemon named by the URL, like login
and serve (tail: means the usual single login); connections to any
number of servers ran through that one daemon. For gomotes, close
destroys the mote-created instances with the named builder type.
An alias works in place of the URL, as does a $GOOS-$GOARCH pair
backed by a gomote.

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
  - tail-name/ is a directory that holds the login credentials for tail://name,
    along with the service socket, lock, and log of the daemon holding that node.

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

/*
IMPLEMENTATION

The protocol between client and server is described in protocol.md.

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
