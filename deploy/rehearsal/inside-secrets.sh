#!/bin/bash
# Runs inside the first rehearsal container (root, no systemd running), after
# provision.sh's first pass, which migrated an earlier server's plain-text
# secrets.env holding the marker TI_TEST_MARKER and created the other secret
# files from their examples, all encrypted with the passphrase TI_TEST_PASS.
# The secret files rest on disk only encrypted, and ti-unlock decrypts them
# after each boot into a root-only tmpfs under /run (owner decision
# 2026-10-22; docs/manual/server.md, "Unlocking the secrets after a boot").
#
# The passphrase is typed by the owner at the terminal. Here it is fed on a
# test-only file descriptor, which ti-secrets honours only where
# TI_REHEARSAL=1 (this container sets it) and refuses anywhere else.
#
# "On disk" is the container's root file system, searched whole (find -xdev:
# /proc, /sys, /dev and every tmpfs left out, the secrets tmpfs and /dev/shm
# among them). The test's own working files that hold a secret are kept in
# /dev/shm, a tmpfs, so the search sees only what the tools wrote.
set -euo pipefail
set +x

fail() { echo "REHEARSAL FAIL: $*" >&2; exit 1; }
pass() { echo "REHEARSAL ok: $*"; }

pass=${TI_TEST_PASS:?}
marker=${TI_TEST_MARKER:?}
etc=/etc/trend-investing
run=/run/trend-investing/secrets
names="secrets.env ibkr-login.env accounts/boost.env accounts/trad-ira.env accounts/roth-ira.env"
p=/root/deploy/provision.sh

# with PASSPHRASE CMD...: CMD, given PASSPHRASE on descriptor 7 for every
# line it reads (a new passphrase and its confirmation are the same line).
with() {
  local phrase=$1
  shift
  TI_PASSPHRASE_FD=7 "$@" 7< <(yes "$phrase")
}
# lines LINE... CMD...: CMD given exactly these lines on descriptor 7.
lines_then() {
  local -a given=()
  while test "$1" != --; do given+=("$1"); shift; done
  shift
  TI_PASSPHRASE_FD=7 "$@" 7< <(printf '%s\n' "${given[@]}")
}
# on_disk TEXT: every file on the root file system holding TEXT.
on_disk() { find / -xdev -type f -print0 2>/dev/null | xargs -0 grep -lF -- "$1" 2>/dev/null || true; }
# sums: the encrypted files' and the unlocked copies' checksums, one per line.
sums() { (cd / && find "${etc#/}" "${run#/}" -type f \( -name '*.gpg' -o -path "${run#/}/*" \) -print0 2>/dev/null | sort -z | xargs -0 -r sha256sum); }
unlocked_files() { find "$run" -mindepth 1 -type f 2>/dev/null | sort; }

{ command -v ti-secrets && command -v ti-unlock; } >/dev/null || fail "ti-secrets and ti-unlock are not installed on root's PATH"
test "$(stat -c %U:%a "$(command -v ti-secrets)")" = root:755 || fail "ti-secrets is not root's, 0755"

# --- Migration: the earlier server's plain-text file is encrypted, checked by
# decrypting it, then shredded and removed; the stubs created from the
# examples are encrypted too, so no secret file rests on disk in plain text.
for n in $names; do
  test ! -e "$etc/$n" || fail "$etc/$n is still on disk in plain text after provisioning"
  test -f "$etc/$n.gpg" || fail "$etc/$n.gpg was not written"
  test "$(stat -c %U:%G:%a "$etc/$n.gpg")" = root:root:600 || fail "$etc/$n.gpg is not root:root 0600"
done
test "$(stat -c %a "$etc/accounts")" = 700 || fail "$etc/accounts is not 0700"
found=$(on_disk "$marker")
test -z "$found" || fail "the migrated secret is still on disk in plain text: $found"
pass "provisioning migrated the earlier plain-text secrets.env and encrypted every stub: no secret file in plain text, the marker nowhere on disk"

# --- Before the unlock: nothing is decrypted, and every unit that loads a
# secret loads it from the tmpfs and asserts it, so systemd refuses to start
# it while locked (the systemd container starts them: inside-systemd.sh).
test -z "$(unlocked_files)" || fail "provisioning left decrypted files in $run: $(unlocked_files)"
for unit in /etc/systemd/system/ti-*.service; do
  while IFS= read -r load; do
    src=${load#*:}
    case $src in "$run"/*) ;; *) fail "$(basename "$unit") loads a credential from $src, outside $run" ;; esac
    grep -qxF "AssertPathExists=$src" "$unit" || fail "$(basename "$unit") loads $src but does not assert it"
  done < <(sed -n 's/^LoadCredential=//p' "$unit")
done
pass "before the unlock nothing is decrypted, and every unit that loads a secret loads it from $run and asserts it"

# --- A wrong passphrase changes nothing, locked or unlocked.
before=$(sums)
err=$(mktemp)
if with "not the passphrase" ti-unlock >/dev/null 2>"$err"; then fail "ti-unlock accepted a wrong passphrase"; fi
grep -qi 'passphrase' "$err" || fail "ti-unlock's refusal does not name the passphrase: $(cat "$err")"
test -z "$(unlocked_files)" || fail "a wrong passphrase left decrypted files: $(unlocked_files)"
test "$(sums)" = "$before" || fail "a wrong passphrase changed an encrypted file"
pass "a wrong passphrase unlocks nothing and changes nothing"

# The descriptor is the rehearsal's only: anywhere else it is refused before
# anything is read.
if env -u TI_REHEARSAL TI_PASSPHRASE_FD=7 ti-unlock >/dev/null 2>"$err" 7< <(yes "$pass"); then fail "ti-unlock read the passphrase from a descriptor without TI_REHEARSAL=1"; fi
grep -q 'TI_REHEARSAL' "$err" || fail "the refusal of the descriptor does not say it is the rehearsal's only: $(cat "$err")"
test -z "$(unlocked_files)" || fail "the refused descriptor unlocked something"
# With no descriptor and no terminal, there is nowhere to read it from.
if setsid -w ti-unlock </dev/null >/dev/null 2>"$err"; then fail "ti-unlock ran with no terminal"; fi
grep -qi 'terminal' "$err" || fail "ti-unlock without a terminal does not say so: $(cat "$err")"
pass "the passphrase descriptor is refused outside the rehearsal, and with no terminal ti-unlock refuses"

# --- After the unlock: the files are decrypted into a root-only tmpfs, and
# the marker is there and still nowhere on disk.
with "$pass" ti-unlock >/dev/null || fail "ti-unlock refused the right passphrase"
case $(stat -f -c %T "$run") in tmpfs | ramfs) ;; *) fail "$run is not a memory file system: $(stat -f -c %T "$run")" ;; esac
test "$(stat -c %U:%G:%a "$run")" = root:root:700 || fail "$run is not root:root 0700"
for n in $names; do
  test "$(stat -c %U:%G:%a "$run/$n")" = root:root:600 || fail "$run/$n is not root:root 0600"
done
test "$(unlocked_files | wc -l)" -eq 5 || fail "the unlock wrote other files: $(unlocked_files)"
grep -qxF "TI_IBKR_FLEX_TOKEN=$marker" "$run/secrets.env" || fail "the unlocked secrets.env does not hold the migrated line"
cmp -s "$run/ibkr-login.env" /root/deploy/etc/ibkr-login.env.example || fail "the unlocked ibkr-login.env is not the example it was created from"
found=$(on_disk "$marker")
test -z "$found" || fail "after the unlock the secret is on disk: $found"
before=$(sums)
if with "not the passphrase" ti-unlock >/dev/null 2>&1; then fail "ti-unlock accepted a wrong passphrase once unlocked"; fi
test "$(sums)" = "$before" || fail "a wrong passphrase changed the unlocked or the encrypted files"
pass "ti-unlock decrypts every file into a root-only tmpfs, 0600, the secret only there; a wrong passphrase then changes nothing"

# --- An edit: decrypted to the tmpfs, the editor run, encrypted again in one
# rename, the unlocked copy replaced, and no plain text left behind. The
# editor stand-in replaces the file with /dev/shm/ti-edit/content and notes
# that it ran.
edits=/dev/shm/ti-edit
install -d -m 0700 "$edits"
cat >/root/ti-editor.sh <<'EDITOR'
#!/bin/sh
echo ran >>/dev/shm/ti-edit/runs
cat /dev/shm/ti-edit/content >"$1"
EDITOR
chmod 0755 /root/ti-editor.sh
edit() { EDITOR=/root/ti-editor.sh with "$1" ti-secrets edit "$2"; }
runs() { if test -e "$edits/runs"; then wc -l <"$edits/runs"; else echo 0; fi; }
marker2=edit-$(head -c 12 /dev/urandom | od -An -tx1 | tr -d ' \n')
{ cat "$run/secrets.env"; echo "TI_GITHUB_ISSUES_TOKEN=$marker2"; } >"$edits/content"
old=$(sha256sum <"$etc/secrets.env.gpg")
edit "$pass" secrets.env >/dev/null || fail "ti-secrets edit secrets.env failed"
test "$(runs)" = 1 || fail "the editor did not run once"
test "$(sha256sum <"$etc/secrets.env.gpg")" != "$old" || fail "the edit did not re-encrypt secrets.env"
test "$(stat -c %U:%G:%a "$etc/secrets.env.gpg")" = root:root:600 || fail "the re-encrypted file is not root:root 0600"
cmp -s "$run/secrets.env" "$edits/content" || fail "the unlocked copy was not replaced by the edit"
test "$(unlocked_files | wc -l)" -eq 5 || fail "the edit left files in the tmpfs: $(unlocked_files)"
test -z "$(find "$run" -mindepth 1 -name '.*')" || fail "the edit left a working folder in the tmpfs: $(find "$run" -mindepth 1 -name '.*')"
test -z "$(find "$etc" -name '*.tmp*')" || fail "the edit left a temporary file beside the encrypted one"
found=$(on_disk "$marker2")
test -z "$found" || fail "the edit left its plain text on disk: $found"
pass "ti-secrets edit decrypts to the tmpfs, runs the editor, re-encrypts in one rename, replaces the unlocked copy and leaves no plain text"

# The edit is in the encrypted file: locked and unlocked again, it is there.
ti-secrets lock >/dev/null || fail "ti-secrets lock failed"
test -z "$(unlocked_files)" || fail "ti-secrets lock left decrypted files: $(unlocked_files)"
with "$pass" ti-unlock >/dev/null || fail "ti-unlock failed after the lock"
grep -qxF "TI_GITHUB_ISSUES_TOKEN=$marker2" "$run/secrets.env" || fail "the edit did not reach the encrypted file"

# An edit with a wrong passphrase runs no editor and changes nothing.
before=$(sums)
if edit "not the passphrase" secrets.env >/dev/null 2>&1; then fail "ti-secrets edit accepted a wrong passphrase"; fi
test "$(runs)" = 1 || fail "a wrong passphrase ran the editor"
test "$(sums)" = "$before" || fail "an edit with a wrong passphrase changed a file"
# An editor that changes nothing writes nothing.
cp "$run/secrets.env" "$edits/content"
edit "$pass" secrets.env >/dev/null || fail "an unchanged edit failed"
test "$(sums)" = "$before" || fail "an unchanged edit re-encrypted the file"
# An edit while locked leaves nothing decrypted.
ti-secrets lock >/dev/null
printf 'IBKR_ACCOUNT_BOOST=DU0000000\n' >"$edits/content"
edit "$pass" accounts/boost.env >/dev/null || fail "an edit while locked failed"
test -z "$(unlocked_files)" || fail "an edit while locked decrypted files into the tmpfs: $(unlocked_files)"
for bad in ../secrets.env /etc/passwd accounts/../../x nothing.env accounts/x.env; do
  if edit "$pass" "$bad" >/dev/null 2>&1; then fail "ti-secrets edit accepted the name '$bad'"; fi
done
with "$pass" ti-unlock >/dev/null
grep -qx 'IBKR_ACCOUNT_BOOST=DU0000000' "$run/accounts/boost.env" || fail "the edit made while locked is not in the encrypted file"
pass "an edit with a wrong passphrase or no change writes nothing; an edit while locked decrypts nothing into the tmpfs; only the five names are accepted"

# --- The passphrase is changed for every file at once; the old one then
# unlocks nothing.
new="a new rehearsal passphrase $(head -c 6 /dev/urandom | od -An -tx1 | tr -d ' \n')"
before=$(sums)
if lines_then "not the passphrase" "$new" "$new" -- ti-secrets passphrase >/dev/null 2>&1; then fail "ti-secrets passphrase accepted a wrong current passphrase"; fi
if lines_then "$pass" "$new" "$new but different" -- ti-secrets passphrase >/dev/null 2>&1; then fail "ti-secrets passphrase accepted a confirmation that differs"; fi
if lines_then "$pass" short short -- ti-secrets passphrase >/dev/null 2>&1; then fail "ti-secrets passphrase accepted a passphrase shorter than 12 characters"; fi
test "$(sums)" = "$before" || fail "a refused passphrase change changed a file"
declare -A was
for n in $names; do was[$n]=$(sha256sum <"$etc/$n.gpg"); done
lines_then "$pass" "$new" "$new" -- ti-secrets passphrase >/dev/null || fail "ti-secrets passphrase failed"
for n in $names; do
  test "$(sha256sum <"$etc/$n.gpg")" != "${was[$n]}" || fail "$n.gpg was not encrypted again with the new passphrase"
done
ti-secrets lock >/dev/null
if with "$pass" ti-unlock >/dev/null 2>&1; then fail "the old passphrase still unlocks after the change"; fi
with "$new" ti-unlock >/dev/null || fail "the new passphrase does not unlock"
lines_then "$new" "$pass" "$pass" -- ti-secrets passphrase >/dev/null || fail "changing the passphrase back failed"
test -z "$(find "$etc" -name '*.tmp*')" || fail "the passphrase change left temporary files"
pass "ti-secrets passphrase re-encrypts every file with the new passphrase; a wrong current one, a differing confirmation or a short one changes nothing"

# --- A migration whose check fails is refused: the plain text is kept, no
# encrypted file is written. The stand-in gpg writes a truncated file when
# it encrypts, so decrypting it to check fails.
marker3=migrate-$(head -c 12 /dev/urandom | od -An -tx1 | tr -d ' \n')
rm -f "$etc/accounts/roth-ira.env.gpg"
printf 'IBKR_ACCOUNT_ROTH_IRA=%s\n' "$marker3" >"$etc/accounts/roth-ira.env"
chmod 0600 "$etc/accounts/roth-ira.env"
stub=$(mktemp -d)
cat >"$stub/gpg" <<'STUB'
#!/bin/bash
case " $* " in
  *" --symmetric "*) /usr/bin/gpg "$@" | head -c 40 ;;
  *) exec /usr/bin/gpg "$@" ;;
esac
STUB
chmod 0755 "$stub/gpg"
if with "$pass" env PATH="$stub:$PATH" "$p" services >/dev/null 2>"$err"; then fail "provisioning went on after a migration that does not decrypt"; fi
grep -qi 'verif\|check' "$err" || fail "the refused migration does not say the check failed: $(cat "$err")"
grep -qxF "IBKR_ACCOUNT_ROTH_IRA=$marker3" "$etc/accounts/roth-ira.env" || fail "a refused migration removed or changed the plain text"
test ! -e "$etc/accounts/roth-ira.env.gpg" || fail "a refused migration left an encrypted file"
test -z "$(find "$etc" -name '*.tmp*')" || fail "a refused migration left temporary files"
rm -rf "$stub"
# A wrong passphrase is refused before anything is migrated.
if with "not the passphrase" "$p" services >/dev/null 2>&1; then fail "provisioning migrated with a wrong passphrase"; fi
grep -qxF "IBKR_ACCOUNT_ROTH_IRA=$marker3" "$etc/accounts/roth-ira.env" || fail "a wrong passphrase removed the plain text"
with "$pass" "$p" services >/dev/null || fail "provisioning did not migrate the account file"
test ! -e "$etc/accounts/roth-ira.env" || fail "the migrated account file is still on disk"
test -f "$etc/accounts/roth-ira.env.gpg" || fail "the account file was not encrypted"
found=$(on_disk "$marker3")
test -z "$found" || fail "the migrated account number is still on disk: $found"
with "$pass" ti-unlock >/dev/null
grep -qxF "IBKR_ACCOUNT_ROTH_IRA=$marker3" "$run/accounts/roth-ira.env" || fail "the migrated account file does not decrypt to what it held"
pass "a migration that does not decrypt is refused with the plain text kept; a wrong passphrase migrates nothing; a good one encrypts, removes the plain text and decrypts to it"

# --- Provisioning again, with nothing to migrate, asks for nothing and
# changes nothing (no descriptor and no terminal: a prompt would fail it).
before=$(sums)
setsid -w "$p" services </dev/null >/dev/null 2>"$err" || fail "a second provisioning with nothing to migrate failed (did it ask for the passphrase?): $(cat "$err")"
test "$(sums)" = "$before" || fail "a second provisioning changed an encrypted file or the unlocked copies"
pass "provisioning again asks for no passphrase and leaves the encrypted and unlocked files as they were"

rm -rf "$edits"
for m in "$marker" "$marker2" "$marker3"; do
  found=$(on_disk "$m")
  test -z "$found" || fail "a secret is on disk at the end: $found"
done
echo "REHEARSAL (secrets) complete"
