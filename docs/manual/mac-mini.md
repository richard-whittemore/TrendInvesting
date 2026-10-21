# The Mac mini

*For the owner. Paper trading on the Mac mini: the programs run in one
Linux container under Docker Desktop. This chapter is built up section by
section as each part lands; the step-by-step setup is
`docs/runbooks/mac-mini-paper.md`.*

## The programs on the Mac mini

The Mac mini is Apple silicon, so its container is arm64 Linux. Paper
trading now runs on the Lightsail server; on the Mac mini this is a local
rehearsal of the same programs on arm64. The provisioner's Mac mini target
(`TI_TARGET=mac-mini`) is what makes the steps below arm64-safe: `base`
skips Tailscale (its pinned tarball is amd64), and `twsapi` uses the
archive below instead of the x86_64 installer. Without that target the
provisioner is Lightsail's and refuses arm64. The rehearsal checks that no
x86_64 file is installed or left on the container's disk; it cannot watch
every short-lived program a step starts, so "nothing x86_64 runs" rests on
those two steps never starting one: the rehearsal runs `base` for real
under the Mac mini target, and the provisioner's own tests check `twsapi`.

**Built for arm64 from the same commit.** `deploy/build-engine.sh --arch
arm64 --out "$HOME/ti-arm64"` builds the engine, the reporter, the dashboard, `operator` and
`journal-query` for the container. They are built from the same commit,
with the same stamp, as the linux/amd64 build for Lightsail (still the
default, unchanged byte for byte). The stamp is what every decision the
engine records names as its build. A tree with uncommitted changes, or with an untracked file, is
refused, so build the arm64 programs into a folder of their own outside the
checkout (`--out`):
`dist/` is what the Lightsail runbook copies to the x86_64 server, and
`provision.sh services` refuses a program in it built for another
architecture, installing nothing. The data job (`deploy/build-data.sh`) is
pure Python and is the same file on both.

**The TWS API on arm64.** IBKR's pinned TWS API installer is for Linux
x86_64 only. It starts its own x86_64 Java, and it accepts no other Java,
so it cannot run in the container. Under the Mac mini target,
`provision.sh twsapi` instead
uses the same release's Mac/Unix archive (10.51.01). That archive is pinned
in `deploy/versions.env` beside the installer, by its own address and
SHA-256, under the same rule. The step downloads the archive, checks its
SHA-256 against the pin and stops on any difference, with nothing
installed. It then unpacks only the archive's Python client, installs it
into the adapter's own environment and writes the install record. On the
Mac mini, the record names the archive's SHA-256. The adapter starts only
if that record names a pinned file of the pinned version and the `ibapi` it
imports is that version. No emulated helper is used (your decision,
2026-10-18).

**Not finished yet:** IBKR's `ibapi` 10.51.01 needs the `protobuf` library
(version 5.29.5), and on this change nothing installs it. Until the protobuf
pin is in, `provision.sh twsapi` stops with "ibapi did not import" and
records nothing. That stop is the fail-closed behaviour, not a fault, and
it happens on Lightsail too.

**How it is checked.** `deploy/mac-mini/rehearse-programs.sh` builds both
architectures into a scratch folder (never `dist/`), checks that their
stamps match, and then, in an arm64 container under the Mac mini target,
checks these things:
- the programs run;
- the engine names the stamp, and stops within 10 seconds when asked;
- the paper rehearsal passes;
- a tampered download and an unpinned file are both refused, changing
  nothing already installed;
- the pinned archive's Python client unpacks;
- no x86_64 file is on the container's disk, and none is running at the end.

What it does **not** yet prove: that `ibapi` imports, that the install
record is written, and that the adapter's pin check passes. Today it ends
with "programs rehearsal passed up to the ibapi import". Only once the
protobuf pin is in does it go on to those checks and end with
"programs rehearsal passed".

### What can go wrong

| What happens | What happens on the Mac mini | What you do |
| --- | --- | --- |
| The programs were built for the wrong architecture (x86_64 programs, for Lightsail) | `provision.sh services` refuses, naming the program and the container's architecture, and installs nothing | Build with `deploy/build-engine.sh --arch arm64` and copy the folder again |
| The tree had uncommitted changes | The build refuses; nothing is built | Commit (or discard) the changes, then build |
| IBKR replaced the archive at its address | `provision.sh twsapi` stops with "checksum mismatch"; nothing is installed and the adapter will not start | Nothing on the Mac; this is a version bump (the Lightsail runbook, "Bumping a pinned version"); the two TWS API pins move together |
| `ibapi` does not import (today: protobuf missing) | `provision.sh twsapi` stops; no install record, so the adapter refuses to start | Wait for the protobuf decision; nothing to type |
| The adapter's environment was changed by hand | Detected only if the change moves `ibapi` out of the environment or changes the version it reports, or the install record no longer names the pin: then the adapter refuses to start. An edit to `ibapi`'s files that keeps its version and place is **not** detected (the files are not hashed) | Run `provision.sh twsapi` again: it rebuilds the environment whole |
| Mac-side programs were copied to the Lightsail server | `provision.sh services` refuses, naming the program and the server's architecture, and installs nothing | Build for Lightsail with `deploy/build-engine.sh` (no `--arch`), copy `dist/` again |
