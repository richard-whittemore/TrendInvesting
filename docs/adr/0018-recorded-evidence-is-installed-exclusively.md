# ADR 0018: Recorded evidence is installed exclusively and never overwritten

- Status: Accepted
- Date: 2026-09-16

## Context

Two artefacts are this platform's evidence: a run's journal (ADR 0017) and its registry entry (ADR 0012). AGENTS.md rule 6 states the rule over both — never rewrite or delete a recorded experiment configuration or its evidence — and ADR 0012 states its purpose, that every result, adopted, rejected and failed, is retained under its configuration hash so a surviving Variant cannot look more special than it is.

What neither records is **how** a file becomes evidence: which system call installs it, what happens when the destination is already taken, and what "recorded" has to mean for a command that reports success. Those decisions were made while `cmd/backtest` was built and lived only in its source comments. That is the wrong home for them twice over. They are decisions rather than explanations of adjacent lines, and `docs/development.md`'s comment standard permits a source comment to cite `docs/methodology/`, an ADR, or a `CONTEXT.md` term — so the code enforcing rule 6 had nothing it was allowed to cite.

Three reviews in a row found defects at exactly this seam, each one a step performed after the commit point without the commit point moving with it. That is what makes the mechanism worth fixing in a record rather than re-deriving each time.

## Decision

### Evidence is installed by hard link, never by rename

A completed temporary file in the destination's own directory is hard-linked into place. `rename(2)` replaces an existing destination silently, so a journal or an entry that appeared while this run was in progress — a concurrent run, a restored backup — would be destroyed by it. `link(2)` fails with `EEXIST` atomically instead, which is what makes the refusal a guarantee rather than a check something can race past. The content is complete before the name exists either way, so an interrupted run leaves no partial file that reads like a complete one.

The temporary file has to be in the destination's own directory: a hard link cannot cross filesystems.

**There is deliberately no overwrite flag.** Moving an existing journal aside is a decision a person should make, and one this command should not offer to make for them.

### A destination already holding byte-identical content is the same record, not a collision

For a registry entry, the bytes about to be installed are compared with the bytes already there. Identical content is the record that is already present: nothing is written, so the never-overwrite rule is untouched, and an install that committed its link and then failed at a later step can be repeated rather than being refused. Anything else under a recorded run id is a different run claiming a recorded id, and is refused, leaving what is recorded exactly as it was.

The comparison is on the bytes, never on a decode of each side: two entries that decode alike can differ on disk, and the registry is committed to git and read as a diff, so what is on disk is what "identical" has to mean.

### The link is the commit point, and every flush after it is a second fact

A file's **content** is durable once the file is synced. The **name** pointing at it is not durable until the directory holding that name is synced, and a directory this command created is itself a name in its parent. So after the link succeeds, every directory whose own entries this install changed is flushed, innermost first: the directory the file was linked into, and the parent of every directory that had to be created to reach it. For the registry, the walk ends at the registry root, which is flushed whether or not this command created it; above the root the directories belong to the operator and to git, not to this command.

Because those flushes happen **after** the commit point, a flush that fails leaves the evidence installed. Both facts are therefore reported and never merged into one failure: the error says the file **is** recorded and where, and the command still exits non-zero. An operator told only that registration failed would record the same run again under another id, and two entries for one run is the corrupted audit trail the registry exists to avoid. For the same reason, whether *this process* installed the journal is carried separately from whether anything went wrong — a run that lost the link race finds another run's journal at its own destination, and must not anchor it.

`fsync` on a directory is unavailable on Windows, where the failure is tolerated rather than failing a command whose work is already done.

### Durability ends at git

`fsync` closes the window between a command reporting success and the next commit. It is not what makes the record durable: committing the journal and the registry to git is (ADR 0017). An operator whose flush failed confirms the entry is readable, then commits, and must not re-record the run.

## Consequences

- Source comments in `cmd/backtest` and `internal/registry` cite this ADR where they previously cited AGENTS.md rule 6, which the comment standard does not permit.
- A rerun that points `-out` at an existing journal is refused before the configuration is read, and the run is not registered: nothing was performed.
- Two runs of one configuration recorded at the same instant can neither interleave nor overwrite one another, and two branches that each recorded a run merge as two additions.
- A future writer of evidence — a report, an exported result set — is expected to install it the same way rather than inventing a second mechanism.
