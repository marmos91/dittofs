# Suites that run beside others

`dt run --parallel N` runs up to N cells at once. A cell runs beside others only when its suite
says `parallel: true` in `test/suites.yaml`; every other cell runs alone. Running alone is the
default because a suite that holds something of the host's fails only when someone passes
`--parallel`, and then in ways that look like flakes. So the field is set once someone has
checked the suite, never by default ([RFC 29 §4.4](rfcs/rfc-29-test-harness.md)).

## What dt keeps apart by itself

Between two cells that both say `parallel: true`, dt still runs one at a time when, from their
needs, which include their fixtures':

- either needs free disk (`disk:N`), since nothing reserves it;
- both need the host's rpcbind (`rpcbind`, a fixture with `rpcbind: host`, or `knfsd`);
- both need the same port (`port:N`, as a fixture's set SMB port or knfsd's 2049).

The run's `{bin}` and `{tools}` are built and laid out once, before any cell starts.

## When a suite may say `parallel: true`

Its cells hold nothing else of the host's. Each of these makes a suite unsafe beside another:

- a fixed port outside its needs: a server, or a test listening on one;
- a container, file or directory with a fixed name;
- a lock, a kernel mount outside its fixture, or a host service other than rpcbind, such as NLM,
  statd, a KDC or a systemd unit;
- free disk it fills without a `disk:N` need;
- a measurement: timings and throughput, as `bench-smoke` and `perf-night` take, are skewed by
  whatever runs beside them;
- state a run of it changes for the next one, as `setup.sh`'s cache lock in `system`, or the
  drive letter the Windows SMB client maps.

## How to check a suite

1. Read its `cmd`, its tests' setup and its fixture for the list above.
2. Run its cells beside each other and beside a suite that already says `parallel: true`, twice:
   `dt run SUITE --parallel 3`, then `dt run GROUP --parallel 3` for a group it is in.
3. Read the logs for a clash: a port in use, a name taken, a file someone else wrote.
4. Add `parallel: true` with a comment that says what was run.

## The suites that say it

| Suite | Why it's safe |
|---|---|
| `lint-go`, `lint-repo`, `shellcheck` | they read the checkout and write nothing outside the cell |
| `unit` | `go test`: no test outside the tagged ones listens on a fixed port or writes a fixed path |
| `pjdfstest` | each cell has its own ports, S3 prefix and mount point; three at once ran green |
