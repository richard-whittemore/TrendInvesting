#!/bin/bash
# Runs inside the first rehearsal container (root, no systemd running), after
# provision.sh's first pass, which migrated an earlier server's plain-text
# secrets.env holding the marker TI_TEST_MARKER and created the other secret
# files from their examples, all encrypted with the passphrase TI_TEST_PASS.
# The secret files rest on disk only encrypted, and ti-unlock decrypts them
# after each boot into a root-only tmpfs under /run (ADR 0028;
# docs/manual/server.md, "Unlocking the secrets after a boot").
#
# The passphrase is typed by the owner at the terminal. Here it is fed on a
# test-only file descriptor, which ti-secrets honours only where
# TI_REHEARSAL=1 (this container sets it) and refuses anywhere else.
#
# "On disk" is the container's root file system, searched whole (find -xdev:
# /proc, /sys, /dev and every tmpfs left out, the secrets tmpfs and /dev/shm
# among them). The test's own working files that hold a secret are kept in
# /dev/shm, a tmpfs, so the search sees only what the tools wrote. It sees
# only the files reachable by name: it cannot show that a block shred left
# behind on the disk is gone, which is why ADR 0028 rests on plain text never
# reaching the disk after the migration, not on shred.
#
# The container may mount (CAP_SYS_ADMIN), so ti-secrets mounts the secrets
# tmpfs itself, noswap, as on the server. The stand-ins below (an editor, mv,
# vim) live outside /dev/shm, which is mounted noexec.
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
# that it ran, the core-dump limit it ran with, and the swap file and backup
# an editor keeps (vim's swap file beside the text, its "directory" starting
# with "."; a backup in HOME), which it writes as a copy of the text.
edits=/dev/shm/ti-edit
install -d -m 0700 "$edits"
cat >/root/ti-editor.sh <<'EDITOR'
#!/bin/sh
echo ran >>/dev/shm/ti-edit/runs
ulimit -c >>/dev/shm/ti-edit/core
swap="$(dirname "$1")/.$(basename "$1").swp"
backup="$HOME/$(basename "$1")~"
cp "$1" "$swap"
cp "$1" "$backup"
printf '%s\n%s\n' "$swap" "$backup" >>/dev/shm/ti-edit/kept
stat -f -c %T "$swap" "$backup" >>/dev/shm/ti-edit/kept-fs
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

# --- The tmpfs: mounted noswap, nosuid, nodev, noexec, 0700, with a size cap.
opts=$(awk -v m="$run" '$2 == m {o = $4} END {print o}' /proc/self/mounts)
for o in nosuid nodev noexec noswap mode=700 size=4096k; do
  case ",$opts," in *",$o,"*) ;; *) fail "the secrets tmpfs is mounted without $o: $opts" ;; esac
done
# A tmpfs already there without noswap is refused: nothing is decrypted into it.
ti-secrets lock >/dev/null
if mountpoint -q "$run"; then fail "ti-secrets lock left the tmpfs mounted"; fi
mount -t tmpfs -o mode=0700,nosuid,nodev,noexec,size=4m ti-test "$run"
if with "$pass" ti-unlock >/dev/null 2>"$err"; then fail "ti-unlock decrypted into a tmpfs mounted without noswap"; fi
grep -qF noswap "$err" || fail "the refusal of a swappable tmpfs does not name noswap: $(cat "$err")"
test -z "$(unlocked_files)" || fail "a swappable tmpfs was given decrypted files: $(unlocked_files)"
umount "$run"
# Active swap is named in a warning; with none there is no warning.
printf 'Filename\tType\tSize\tUsed\tPriority\n/swap.img\tfile\t1048572\t0\t-2\n' >/dev/shm/ti-swaps
TI_PROC_SWAPS=/dev/shm/ti-swaps with "$pass" ti-unlock >/dev/null 2>"$err" || fail "ti-unlock refused with swap on: $(cat "$err")"
grep -qF /swap.img "$err" || fail "ti-unlock did not warn that the swap /swap.img is on: $(cat "$err")"
printf 'Filename\tType\tSize\tUsed\tPriority\n' >/dev/shm/ti-swaps
TI_PROC_SWAPS=/dev/shm/ti-swaps with "$pass" ti-unlock >/dev/null 2>"$err" || fail "ti-unlock refused with no swap"
if grep -qi swap "$err"; then fail "ti-unlock warned of swap with none on: $(cat "$err")"; fi
rm -f /dev/shm/ti-swaps
pass "the secrets tmpfs is mounted noswap, nosuid, nodev, noexec, 0700 and capped; one without noswap is refused; active swap is named"

# --- One command at a time: while another holds the lock, every subcommand
# refuses and changes nothing.
lockf=/run/trend-investing/ti-secrets.lock
before=$(sums)
runs_before=$(runs)
exec 6>>"$lockf"
flock -n 6 || fail "could not take $lockf"
for args in status lock unlock migrate passphrase "edit secrets.env" "diff secrets.env"; do
  # shellcheck disable=SC2086
  if EDITOR=/root/ti-editor.sh with "$pass" ti-secrets $args >/dev/null 2>"$err"; then fail "ti-secrets $args ran while another held the lock"; fi
  grep -qi 'another' "$err" || fail "ti-secrets $args, refused for the lock, does not say another is running: $(cat "$err")"
done
if with "$pass" ti-unlock >/dev/null 2>"$err"; then fail "ti-unlock ran while another held the lock"; fi
exec 6>&-
test "$(sums)" = "$before" || fail "a command refused for the lock changed a file"
test "$(runs)" = "$runs_before" || fail "an edit refused for the lock ran the editor"
pass "every ti-secrets subcommand and ti-unlock refuse while another holds the lock"

# --- Temporary files are the command's own: another's beside the encrypted
# file is left alone.
printf 'decoy\n' >"$etc/secrets.env.gpg.tmp"
cp "$run/secrets.env" "$edits/content"
echo "TI_DECOY_CHECK=1" >>"$edits/content"
(ulimit -c unlimited && edit "$pass" secrets.env >/dev/null) || fail "an edit beside another's temporary file failed"
test "$(cat "$etc/secrets.env.gpg.tmp" 2>/dev/null)" = decoy || fail "the edit removed or replaced another command's temporary file"
rm -f "$etc/secrets.env.gpg.tmp"
pass "an edit's temporary file is its own (mktemp); another's is left alone"

# --- The editor: no core dumps, and its swap and backup files only in the
# tmpfs, removed after.
test "$(tail -n 1 "$edits/core")" = 0 || fail "the editor ran with core dumps allowed: ulimit -c is $(tail -n 1 "$edits/core")"
test -s "$edits/kept" || fail "the editor stand-in noted no swap or backup file"
while IFS= read -r k; do
  case $k in "$run"/*) ;; *) fail "the editor kept $k outside the tmpfs" ;; esac
  test ! -e "$k" || fail "the editor's $k was left after the edit"
done <"$edits/kept"
if grep -qv '^tmpfs$' "$edits/kept-fs"; then fail "the editor's swap or backup file was on $(sort -u "$edits/kept-fs" | tr '\n' ' ')"; fi
# vim is given no swap file (-n) and its swap and backup folders in the tmpfs.
vimdir=$(mktemp -d)
cat >"$vimdir/vim" <<'VIM'
#!/bin/sh
printf '%s\n' "$@" >/dev/shm/ti-edit/vim-args
for last; do :; done
cat /dev/shm/ti-edit/content >"$last"
VIM
chmod 0755 "$vimdir/vim"
cp "$run/secrets.env" "$edits/content"
echo "TI_VIM_CHECK=1" >>"$edits/content"
EDITOR=vim PATH="$vimdir:$PATH" with "$pass" ti-secrets edit secrets.env >/dev/null || fail "an edit with vim failed"
grep -qx -- -n "$edits/vim-args" || fail "vim was not given -n (no swap file): $(cat "$edits/vim-args")"
grep -q "directory=$run/" "$edits/vim-args" || fail "vim's swap folder is not in the tmpfs: $(cat "$edits/vim-args")"
grep -q "backupdir=$run/" "$edits/vim-args" || fail "vim's backup folder is not in the tmpfs: $(cat "$edits/vim-args")"
rm -rf "$vimdir"
pass "the editor runs with no core dumps; its swap and backup files stay in the tmpfs and go with the edit; vim is given -n and the tmpfs for its folders"

# --- An edit cut short after the encrypted file is written never leaves the
# unlocked copy older than it; an unchanged edit then brings it up to date.
mvdir=$(mktemp -d)
cat >"$mvdir/mv" <<'MV'
#!/bin/bash
for last; do :; done
case $last in /run/trend-investing/secrets/*) echo "stand-in mv: refused" >&2; exit 1 ;; esac
exec /usr/bin/mv "$@"
MV
chmod 0755 "$mvdir/mv"
oldcopy=$(cat "$run/secrets.env")
cp "$run/secrets.env" "$edits/content"
echo "TI_CUT_SHORT=1" >>"$edits/content"
if EDITOR=/root/ti-editor.sh PATH="$mvdir:$PATH" with "$pass" ti-secrets edit secrets.env >/dev/null 2>&1; then fail "the edit went on when its unlocked copy could not be placed"; fi
rm -rf "$mvdir"
if test -f "$run/secrets.env" && test "$(cat "$run/secrets.env")" = "$oldcopy"; then fail "an edit cut short left the unlocked copy older than the encrypted file"; fi
edit "$pass" secrets.env >/dev/null || fail "the unchanged edit after one cut short failed"
cmp -s "$run/secrets.env" "$edits/content" || fail "an unchanged edit did not bring the unlocked copy up to the encrypted file"
pass "an edit cut short leaves no unlocked copy older than the encrypted file; an unchanged edit brings it up to date"

# --- A migration conflict names a command that shows the difference, and it works.
printf 'TI_CONFLICT=plain\n' >"$etc/secrets.env"
if with "$pass" ti-secrets migrate >/dev/null 2>"$err"; then fail "migrate went on with two differing copies"; fi
grep -qF "ti-secrets diff secrets.env" "$err" || fail "the conflict does not name ti-secrets diff: $(cat "$err")"
with "$pass" ti-secrets diff secrets.env >"$edits/diff" 2>"$err" || fail "ti-secrets diff failed: $(cat "$err")"
grep -qx '+TI_CONFLICT=plain' "$edits/diff" || fail "ti-secrets diff does not show the plain-text line"
grep -q '^-TI_CUT_SHORT=1' "$edits/diff" || fail "ti-secrets diff does not show the encrypted copy's line"
shred -u "$etc/secrets.env"
pass "a migration conflict names ti-secrets diff, which shows both copies"

# --- A passphrase change cut short mid-rename is resumed with both passphrases.
new="a resumed rehearsal passphrase $(head -c 6 /dev/urandom | od -An -tx1 | tr -d ' \n')"
mvdir=$(mktemp -d)
cat >"$mvdir/mv" <<'MV'
#!/bin/bash
for last; do :; done
case $last in
  *.gpg)
    n=$(($(cat /dev/shm/ti-mv-count 2>/dev/null || echo 0) + 1))
    echo "$n" >/dev/shm/ti-mv-count
    if test "$n" -eq 3; then echo "stand-in mv: refused" >&2; exit 1; fi ;;
esac
exec /usr/bin/mv "$@"
MV
chmod 0755 "$mvdir/mv"
rm -f /dev/shm/ti-mv-count
if PATH="$mvdir:$PATH" lines_then "$pass" "$new" "$new" -- ti-secrets passphrase >/dev/null 2>&1; then fail "the passphrase change went on when a rename failed"; fi
rm -rf "$mvdir" /dev/shm/ti-mv-count
test -f "$etc/.passphrase-change" || fail "the cut-short passphrase change left no journal"
if with "$pass" ti-unlock >/dev/null 2>"$err"; then fail "ti-unlock ran mid passphrase change"; fi
grep -qF -- "--resume" "$err" || fail "ti-unlock mid passphrase change does not name --resume: $(cat "$err")"
lines_then "$pass" "$new" "$new" -- ti-secrets passphrase --resume >/dev/null 2>"$err" || fail "ti-secrets passphrase --resume failed: $(cat "$err")"
if test -e "$etc/.passphrase-change"; then fail "the resumed change left its journal"; fi
test -z "$(find "$etc" -name '*.tmp*')" || fail "the resumed change left temporary files: $(find "$etc" -name '*.tmp*')"
ti-secrets lock >/dev/null
if with "$pass" ti-unlock >/dev/null 2>&1; then fail "the old passphrase unlocks after the resumed change"; fi
with "$new" ti-unlock >/dev/null || fail "the new passphrase does not unlock after the resumed change"
lines_then "$new" "$pass" "$pass" -- ti-secrets passphrase >/dev/null || fail "changing the passphrase back failed"
pass "a passphrase change cut short is resumed with ti-secrets passphrase --resume; then only the new passphrase unlocks"

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
