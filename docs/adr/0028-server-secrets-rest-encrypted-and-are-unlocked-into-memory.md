# ADR 0028: The server's secrets rest on disk only encrypted, and are unlocked into memory after each boot

- Status: Accepted (2026-10-22, by the owner)
- Date: 2026-10-22
- Relates to: ADR 0025 (the IBKR adapter, whose login and account numbers these files hold)

## In plain English

*A summary. Where it differs from the text below, the text governs.*

The server's secret files (the IBKR login, the account numbers, the market
data key, the alert keys, the Flex token) are kept on disk only encrypted,
with a passphrase the owner chooses and types. Nothing on the server knows
the passphrase. After every boot the owner types it once (`sudo ti-unlock`),
and the files are decrypted into memory, never onto the disk. Until then
every program that needs a secret refuses to start.

## Context

The server is a Lightsail instance. A copy of its disk or a Lightsail
snapshot is the likeliest way for a secret to leak: snapshots are kept for
months and copied between regions. Lightsail offers no hardware key store
(TPM), so any key the server could use by itself (systemd-creds, an
unattended keyring) would sit on the same disk as the files it unlocks, and
a snapshot would carry both.

## Decision

1. Every secret file under `/etc/trend-investing` rests on disk only as an
   OpenPGP symmetric encryption of it (AES-256, iterated and salted S2K,
   modification detection), all with one passphrase of at least 12
   characters. The passphrase is stored nowhere and there is no recovery.
2. After each boot the owner unlocks the files into a root-only tmpfs under
   `/run/trend-investing/secrets`, mounted `noswap` (with nosuid, nodev,
   noexec and a size cap) so the plain text is never written to the disk or
   to swap. If that mount cannot be made, nothing is decrypted. `noswap`
   covers the tmpfs only: the passphrase and the plain text also pass
   through the memory of the tool, gpg and the editor, which the kernel can
   page to swap on the disk. So nothing reads the passphrase or decrypts
   while any swap is on, or while the list of active swap cannot be read.
3. Every unit that loads a secret loads it from that tmpfs and asserts the
   file exists, so systemd refuses to start it while the secrets are locked.
   The reporter therefore cannot run while locked; its outside heartbeat stops
   and pages the owner.
4. The plain text of a file being edited, decrypted or re-encrypted lives only
   in that tmpfs, and is removed when the command ends.
5. A plain-text file left from before this decision is encrypted, checked by
   decrypting it, and then shredded. Shredding is not a guarantee on a
   journaling file system or on copy-on-write or SSD storage, which a cloud
   volume is: the protection this decision gives is that plain text never
   touches the disk after the migration, so snapshots taken before it must be
   deleted and the secrets that were on the disk in plain text should be
   rotated.

## Consequences

- A reboot leaves the system unable to trade until the owner unlocks it.
  This is the price of keeping the key off the disk.
- Losing the passphrase loses every secret; the owner must fill them in
  again from the brokers and services.
- `deploy/secrets.sh` (installed as `ti-secrets` and `ti-unlock`) is the only
  tool that reads or writes the encrypted files; `docs/manual/server.md`,
  "Where the secrets live", is the owner's description of it.
