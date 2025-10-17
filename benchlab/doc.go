// Copyright 2025 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

/*
Benchlab is a benchmark lab.
It runs benchmarks at a variety of commits on a variety of machines
and presents the results.

Usage:

	benchlab [-commit=HEAD,HEAD^] [-host=local] \
		[-pkg=.] [-reps=R] [-run=.] \
		[-bench=.] [-benchtime=500ms] [-count=5] [-cpu=N] \

Benchlab starts by building the test at the given list of commits
(the default is the current Git head and its parent)
for the given list of hosts where the benchmark should run.
The commit list is a comma-separated list of git commit ranges;
the default of “HEAD,HEAD^” is equivalent to the Git range syntax “HEAD^^..”.

The host “local” (the default) denotes the local system.
The syntax for remote hosts is described below; usually
an ssh'able hostname or gomote system description suffices.

After building all the test binaries, benchlab copies the test binaries to
the target hosts and runs the tests for each commit on each host
(passing the -run flag through),
to avoid benchmarking broken code.

If the tests pass on a given host, then benchlab runs the benchmarks
for each commit, passing the -bench, -benchtime, and -count flags through.
It repeats that list of commits the number of times specified by the -reps flag (default 4).

Note that the defaults for -bench, -benchtime, and -count are different
from the go command's. In the default configuration, all benchmarks run,
the default per-benchmark target time is 500ms (not 2s),
and the default benchmark count is 5 (not 1).
Along with the default -reps=4, this is a total of 20 samples
for each benchmark on each host.

When the -cpu flag is specified, it must be a single number, not a list.
By default, benchlab only runs one benchmark at a time on each machine.
If the -cpu=P flag is given, then benchlab allows each machine to run N/P
simultaneous benchmarks, where N is the number of CPUs on the machine,
and benchlab passes -cpu=P through to the benchmark binary.
This allows making better use of high-core systems, especially when
running single-threaded benchmarks, at the cost of some potential
measurement noise.

After each individual benchmark run finishes, benchlab appends
the raw results to an output file named bench.YYYY-MM-DD[.N].
The .N is added automatically (starting at N ≥ 1) to avoid overwriting
existing output files. While benchlab runs, it also updates the output file
benchstat.YYYY-MM-DD[.N] with benchstat output summarizing the
runs so far.

Benchlab stores a cache of built test binaries and test and benchmark
results in the directory “./.benchlab”. This speeds subsequent runs
and allows, for example, adding a new host or commit to an experiment
without repeating all the previous work. The -a flag forces benchlab to
ignore all cached results, although it still writes its work to the cache
for use by future runs.

# Host Name Syntax

The access mechanisms for a host depend on the syntax of the name.

The name “local” denotes the local machine, which is accessed by
running commands directly.

A name of the form “goos-goarch” denotes a freshly created gomote
of the given type (typically “gotip-goos-goarch” although benchlab
understands special cases like resolving “darwin-arm64” to the gomote
type “gotip-darwin-arm64_15”. Adding an additional _suffix or -suffix
to the name restricts the choice to exactly that type.

Other names are assumed to be valid systems accessible by “ssh” and “scp”.
Binaries are copied to the default home directory on the system and
are named benchlab.HASH.exe for some hexadecimal hash value.
Benchlab deletes the test binaries when it finishes (TODO),
but if benchlab is interrupted or crashes, it may leave them behind.
To determine the operating system and architecture of the remote system,
benchlab runs the “uname” and “uname -m” commands over an initial ssh
connection.

Additional build settings can be specified in a host name
by adding one or more :key=value suffixes to the name,
as in “local:GOAMD64=v2” or “local:tags=new”.
Upper-case keys are used as environment variables,
and lower-case keys are used as “go build” flags.

# Examples

Benchmark the current package's benchmarks on the local system,
comparing the current Git commit to the previous Git commit:

	benchlab

Benchmark on the local system,
the ssh'able system kremvax, and a linux-amd64 gomote,
allowing each host to run simultaneous benchmarks for every
pair of (potentially hyperthreaded) CPUs it has:

	benchlab -host=local,kremvax,linux-amd64 -cpu=2

Benchmark all commits from 123def to the current branch head:

	benchlab -commit=123def^..

Benchmark the last three commits:

	benchlab -commit=HEAD^^^..

Benchmark only the current commit, varying the GOAMD64 setting:

	benchlab -commit=HEAD -host=local:GOAMD64=v2,local:GOAMD64=v3

Watch benchstat output update in a terminal:

	tail -F benchstat.2025-10-15.2
*/
package main
