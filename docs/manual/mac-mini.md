# The Mac mini

*For the owner. Paper trading on the Mac mini: the programs run in one
Linux container under Docker Desktop. This chapter is built up section by
section as each part lands; the step-by-step setup is
`docs/runbooks/mac-mini-paper.md`.*

## The programs on the Mac mini

The Mac mini is Apple silicon, so its container is arm64 Linux. Everything
in it runs natively, with no emulation, day to day or at install.

**Built for arm64 from the same commit.** `deploy/build-engine.sh --arch
arm64` builds the engine, the reporter, the dashboard, `operator` and
`journal-query` for the container. They are built from the same commit,
with the same stamp, as the linux/amd64 build for Lightsail (still the
default, unchanged byte for byte). The stamp is what every decision the
engine records names as its build. A tree with uncommitted changes is
refused. The data job (`deploy/build-data.sh`) is pure Python and is the
same file on both.

**The TWS API on arm64.** IBKR's pinned TWS API installer is for Linux
x86_64 only. It starts its own x86_64 Java, and it accepts no other Java,
so it cannot run in the container. On arm64, `provision.sh twsapi` instead
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
(version 5.29.5), and nothing installs it yet. Until that is decided and
pinned, `provision.sh twsapi` stops with "ibapi did not import" and records
nothing. That stop is the fail-closed behaviour, not a fault, and it
happens on Lightsail too.

**How it is checked.** `deploy/mac-mini/rehearse-programs.sh` builds both
architectures, checks that their stamps match, and then, in an arm64
container, checks these things:
- the programs run;
- the engine names the stamp;
- the paper rehearsal passes;
- a tampered download and an unpinned file are both refused;
- the pinned archive installs and the adapter's pin check passes;
- no x86_64 program runs.

### What can go wrong

| What happens | What happens on the Mac mini | What you do |
| --- | --- | --- |
| The programs were built for the wrong architecture | They do not start in the container ("exec format error"); nothing runs | Build with `deploy/build-engine.sh --arch arm64` and install again |
| The tree had uncommitted changes | The build refuses; nothing is built | Commit (or discard) the changes, then build |
| IBKR replaced the archive at its address | `provision.sh twsapi` stops with "checksum mismatch"; nothing is installed and the adapter will not start | Nothing on the Mac; this is a version bump (the Lightsail runbook, "Bumping a pinned version"); the two TWS API pins move together |
| `ibapi` does not import (today: protobuf missing) | `provision.sh twsapi` stops; no install record, so the adapter refuses to start | Wait for the protobuf decision; nothing to type |
| The adapter's environment was changed by hand | The adapter refuses to start (its `ibapi` is not the recorded one) | Run `provision.sh twsapi` again |
