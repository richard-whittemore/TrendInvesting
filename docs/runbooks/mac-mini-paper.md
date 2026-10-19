# Mac mini paper deployment

*The step-by-step setup for paper trading on the Mac mini. Steps are added
as each part lands; what each part does is in `docs/manual/mac-mini.md`.*

## Build and install the programs

Done by the deployer from a clean checkout outside the iCloud Desktop. You
type nothing.

1. Build the programs for the container: `deploy/build-engine.sh --arch
   arm64` and `deploy/build-data.sh`. Each prints the stamp. A dirty tree is
   refused.
2. Rehearse them: `deploy/mac-mini/rehearse-programs.sh`. It must end with
   `programs rehearsal passed`. To run it in the Mac mini's own container
   before its first run, add `--container NAME`.
3. Install the TWS API in the container: `provision.sh twsapi`. It fetches
   the pinned Mac/Unix archive and verifies its SHA-256 (a mismatch stops
   it, with nothing installed). Then run `provision.sh adapter`.
   **Blocked today:** `ibapi` 10.51.01 needs protobuf 5.29.5, which is not
   yet pinned, so step 3 stops at "ibapi did not import".
