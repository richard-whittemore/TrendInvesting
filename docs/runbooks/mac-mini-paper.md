# Mac mini paper deployment

*The step-by-step setup for the Mac mini's arm64 container. Paper trading
runs on the Lightsail server (`docs/runbooks/lightsail-deployment.md`); on
the Mac mini this is a local rehearsal. Steps are added as each part lands;
what each part does is in `docs/manual/mac-mini.md`. Every `provision.sh`
step here runs with `TI_TARGET=mac-mini`.*

## Build and install the programs

Done by the deployer from a clean checkout outside the iCloud Desktop. You
type nothing.

1. Build the programs for the container into a folder of their own, never
   `dist/` (Lightsail's), and outside the checkout, since an untracked folder
   in it makes the tree dirty and the build refuses: `deploy/build-engine.sh
   --arch arm64 --out "$HOME/ti-arm64"` and `deploy/build-data.sh
   "$HOME/ti-arm64/ti-data"`. Each prints the stamp. A dirty tree is
   refused.
2. Rehearse them: `deploy/mac-mini/rehearse-programs.sh` (it builds into a
   scratch folder of its own). **Today** it ends with `programs rehearsal
   passed up to the ibapi import`: `ibapi` does not import until the
   protobuf pin is in, so the install record and the adapter's pin check
   are not yet verified. Once the protobuf pin and this change are both in,
   it must end with `programs rehearsal passed`. To run it in the Mac
   mini's own container, add `--container NAME`; it can be run there again,
   but only while the adapter has not yet been started in that container
   (it rebuilds the TWS API tree and the adapter's environment).
3. Install the TWS API in the container: `TI_TARGET=mac-mini provision.sh
   twsapi`. It fetches the pinned Mac/Unix archive and verifies its SHA-256
   (a mismatch stops it, with nothing installed). Then run `TI_TARGET=mac-mini
   provision.sh adapter`.
   **Blocked today:** `ibapi` 10.51.01 needs protobuf 5.29.5, which is not
   yet pinned, so step 3 stops at "ibapi did not import".
