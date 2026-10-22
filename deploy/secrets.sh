#!/bin/bash
# ti-secrets and ti-unlock: the server's secret files rest on disk only
# encrypted, with a passphrase the owner chooses and types; after each boot
# the owner unlocks them into a root-only tmpfs, which the units load their
# credentials from (ADR 0028). Lightsail offers no TPM, so a
# key systemd-creds could seal them with would sit in
# /var/lib/systemd/credential.secret on the same disk, and a disk image or
# snapshot would carry both: the key here is the passphrase, which is never
# stored (docs/manual/server.md, "Where the secrets live").
#
#   sudo ti-unlock                 ask for the passphrase and decrypt every
#                                  secret file into /run/trend-investing/secrets
#                                  (the same as ti-secrets unlock); a wrong
#                                  passphrase changes nothing. Then start the
#                                  Gateway if it is enabled, which it could not
#                                  do at boot.
#   sudo ti-secrets edit NAME      decrypt NAME into the tmpfs, run the editor
#                                  on it, encrypt it again (a temporary file,
#                                  checked by decrypting it, synced, renamed
#                                  over the old one), replace the unlocked copy
#                                  if the secrets are unlocked, and remove the
#                                  plain text
#   sudo ti-secrets passphrase     change the passphrase of every file: every
#                                  new copy is written and checked by
#                                  decrypting it with the new passphrase
#                                  before any is renamed over its file, and a
#                                  journal records the progress
#   sudo ti-secrets passphrase --resume
#                                  finish a change cut short (a crash, a
#                                  failed rename): asks for the old and the
#                                  new passphrase, and leaves every file under
#                                  the new one
#   sudo ti-secrets lock           remove the decrypted copies (a running unit
#                                  keeps the copy systemd handed it)
#   sudo ti-secrets status         which files are encrypted and which unlocked;
#                                  asks for nothing, prints no secret
#   sudo ti-secrets migrate        encrypt every secret file still in plain text
#                                  (provision.sh services runs it): each is
#                                  encrypted, checked by decrypting it, then
#                                  shredded and removed; a check that fails
#                                  stops it with the plain text kept. Shredding
#                                  is best effort only: on a journaling file
#                                  system or SSD or copy-on-write storage (a
#                                  cloud volume) the old blocks may survive it.
#                                  The protection is that plain text never
#                                  touches the disk after the migration; delete
#                                  the snapshots taken before it, and rotate
#                                  the secrets that were on it (ADR 0028)
#   sudo ti-secrets diff NAME      show how NAME's plain-text file and its
#                                  encrypted copy differ, when migrate finds
#                                  both (it prints both to the terminal)
#
# Every command takes one exclusive lock (flock on
# /run/trend-investing/ti-secrets.lock, outside the secrets tmpfs) for its
# whole run, cleanup included; a second command refuses while one runs.
# Temporary files are made with mktemp beside the file they replace.
#
# NAME is a file's place under /etc/trend-investing: secrets.env,
# ibkr-login.env or accounts/<account>.env. Its encrypted copy is NAME.gpg
# there, root:root 0600; its unlocked copy is
# /run/trend-investing/secrets/NAME, root:root 0600, in a tmpfs mounted
# root-only (0700) for it.
#
# The encryption: GnuPG's symmetric OpenPGP encryption (Ubuntu 24.04's gnupg
# package, installed by provision.sh base): AES-256, the passphrase stretched
# by the iterated and salted S2K with SHA-512 over the largest count OpenPGP
# can express, and the modification detection code, so a wrong passphrase or
# a changed file fails to decrypt rather than giving wrong text. Every file
# shares the one passphrase.
#
# The passphrase is read from the terminal with echo off, held in this
# shell's memory only and handed to gpg on a pipe (--passphrase-fd): never in
# a command line, an environment variable, a file or a log. gpg runs with a
# throwaway home in the tmpfs, starts no agent and caches nothing. The editor
# runs with its home and temporary folder in the tmpfs too, so its swap,
# backup and history files are removed with the plain text; vim is also given
# -n (no swap file) and the tmpfs as its directory and backupdir, nano
# --nobackup. Core dumps are off (ulimit -c 0) and tracing is off (set +x)
# for this script and everything it runs, so no passphrase or plain text is
# written to a core file or a trace.
#
# The tmpfs is mounted noswap (Linux 6.4 and later; Ubuntu 24.04 runs 6.8),
# nosuid, nodev, noexec, mode 0700 and 4 MiB at most; if that mount fails,
# or a tmpfs already there lacks one of these, nothing is decrypted. Swap
# that is on is named in a warning: gpg, the editor and this shell can still
# be paged to it.
#
# TI_PASSPHRASE_FD=N reads the passphrase lines from descriptor N instead of
# the terminal: the rehearsal's only, honoured where TI_REHEARSAL=1 and
# refused anywhere else. TI_PROC_SWAPS=FILE reads the active swap from FILE
# instead of /proc/swaps, also the rehearsal's only.
set +x
ulimit -c 0
set -euo pipefail
umask 077

etc=/etc/trend-investing
run=/run/trend-investing/secrets
lockfile=/run/trend-investing/ti-secrets.lock
# The journal of a passphrase change: the names and temporary files it has
# written and renamed, no secret. While it exists, only --resume runs.
journal=$etc/.passphrase-change
min_length=12
# AES-256, iterated and salted S2K (mode 3) with SHA-512 over 65011712 bytes
# (the largest count OpenPGP's one-byte encoding holds), no compression.
encrypt_options=(--symmetric --cipher-algo AES256 --s2k-mode 3 --s2k-digest-algo SHA512 --s2k-count 65011712 --compress-algo none)

case $(basename "$0") in
  ti-unlock) prog=ti-unlock ;;
  *) prog=ti-secrets ;;
esac

log() { printf '%s: %s\n' "$prog" "$*"; }
die() {
  printf '%s: %s\n' "$prog" "$*" >&2
  exit 1
}
warn() { printf '%s: warning: %s\n' "$prog" "$*" >&2; }

usage() {
  die "usage: ti-unlock, or ti-secrets unlock|lock|status|migrate|passphrase, ti-secrets passphrase --resume, or ti-secrets edit|diff NAME (secrets.env, ibkr-login.env, accounts/<account>.env)"
}

passfd=
if test -n "${TI_PASSPHRASE_FD:-}"; then
  test "${TI_REHEARSAL:-0}" = 1 || die "TI_PASSPHRASE_FD is the rehearsal's only (TI_REHEARSAL=1); on the server the passphrase is typed at the terminal"
  case $TI_PASSPHRASE_FD in '' | *[!0-9]*) die "TI_PASSPHRASE_FD must be a descriptor number" ;; esac
  passfd=$TI_PASSPHRASE_FD
fi
swaps=/proc/swaps
if test -n "${TI_PROC_SWAPS:-}"; then
  test "${TI_REHEARSAL:-0}" = 1 || die "TI_PROC_SWAPS is the rehearsal's only (TI_REHEARSAL=1)"
  swaps=$TI_PROC_SWAPS
fi

# Folders made in the tmpfs for one run (the staging of decrypted files, gpg's
# home, an edit), and temporary encrypted files beside the real ones: all
# removed when the script ends, however it ends.
# A passphrase change that stops before its first rename has changed no
# file, so its journal goes too (drop_journal=1); from the first rename on,
# the journal stays for --resume.
works=()
tmps=()
drop_journal=0
cleanup() {
  local f
  for f in ${tmps[@]+"${tmps[@]}"}; do rm -f -- "$f"; done
  for f in ${works[@]+"${works[@]}"}; do rm -rf -- "$f"; done
  if test "$drop_journal" = 1; then rm -f -- "$journal"; fi
}
trap cleanup EXIT

need_root() { test "$(id -u)" -eq 0 || die "run as root (sudo $prog ...)"; }

# take_lock: the one exclusive lock every command holds until it exits (the
# descriptor closes after the cleanup), on a file outside the secrets tmpfs
# so that lock and its unmount cannot remove it. A second command refuses
# rather than waits: an edit can hold it for as long as the editor is open.
lockfd=
take_lock() {
  install -d -o root -g root -m 0755 /run/trend-investing
  test ! -L "$lockfile" || die "$lockfile is a link; nothing changed"
  exec {lockfd}>>"$lockfile"
  flock -n "$lockfd" || die "another ti-secrets or ti-unlock is running (it holds $lockfile): wait for it to finish, then run this again; nothing changed"
}

need_gpg() { command -v gpg >/dev/null || die "gpg is not installed: run sudo deploy/provision.sh base"; }

# valid_name NAME: one of the secret files this script keeps.
valid_name() {
  case $1 in
    secrets.env | ibkr-login.env) return 0 ;;
    accounts/*.env)
      local a=${1#accounts/}
      a=${a%.env}
      [[ $a =~ ^[a-z0-9][a-z0-9-]*$ ]]
      ;;
    *) return 1 ;;
  esac
}

# encrypted_names, plain_names: the secret files on disk encrypted, and in
# plain text, one NAME per line.
encrypted_names() {
  local n f
  for n in secrets.env ibkr-login.env; do
    if test -f "$etc/$n.gpg"; then echo "$n"; fi
  done
  for f in "$etc"/accounts/*.env.gpg; do
    test -f "$f" || continue
    n=accounts/$(basename "$f" .gpg)
    if valid_name "$n"; then echo "$n"; fi
  done
}
plain_names() {
  local n f
  for n in secrets.env ibkr-login.env; do
    if test -f "$etc/$n"; then echo "$n"; fi
  done
  for f in "$etc"/accounts/*.env; do
    test -f "$f" || continue
    n=accounts/$(basename "$f")
    if valid_name "$n"; then echo "$n"; fi
  done
}

# noswap_supported: this kernel's tmpfs takes the noswap option (Linux 6.4
# and later).
noswap_supported() {
  local r major minor
  r=$(uname -r)
  major=${r%%.*}
  minor=${r#*.}
  minor=${minor%%[!0-9]*}
  [[ $major =~ ^[0-9]+$ && $minor =~ ^[0-9]+$ ]] || return 1
  test "$major" -gt 6 || { test "$major" -eq 6 && test "$minor" -ge 4; }
}

# active_swap: the swap files and partitions that are on, space-separated.
active_swap() { awk 'NR > 1 && NF {printf "%s ", $1}' "$swaps" 2>/dev/null || true; }

# ensure_tmpfs: $run, a tmpfs of its own, root:root 0700, mounted noswap
# (where the kernel has it), nosuid, nodev, noexec and size-capped. A mount
# that fails, or a tmpfs already there without these, is refused, so nothing
# is decrypted onto a disk or into swap. Swap that is on is named.
ensure_tmpfs() {
  local opts=mode=0700,uid=0,gid=0,nosuid,nodev,noexec,size=4m need=(nosuid nodev noexec) have o swap
  test ! -L "$run" || die "$run is a link; nothing decrypted"
  install -d -o root -g root -m 0755 /run/trend-investing
  if ! test -d "$run"; then install -d -o root -g root -m 0700 "$run"; fi
  swap=$(active_swap)
  if noswap_supported; then
    opts=$opts,noswap
    need+=(noswap)
  elif test -n "$swap"; then
    die "this kernel ($(uname -r)) cannot keep a tmpfs out of swap (noswap), and swap is on (${swap% }): turn it off (sudo swapoff -a, and remove it from /etc/fstab) or run Linux 6.4 or later; nothing decrypted"
  fi
  if ! mountpoint -q "$run"; then
    mount -t tmpfs -o "$opts" ti-secrets "$run" 2>/dev/null ||
      die "cannot mount a tmpfs at $run with $opts (the decrypted secrets must stay in memory, never on disk or in swap); nothing decrypted"
  fi
  test "$(stat -f -c %T "$run")" = tmpfs || die "$run is not a tmpfs ($(stat -f -c %T "$run")); nothing decrypted"
  have=$(awk -v m="$run" '$2 == m {o = $4} END {print o}' /proc/self/mounts)
  for o in "${need[@]}"; do
    case ",$have," in
      *",$o,"*) ;;
      *) die "$run is mounted without $o ($have): unmount it (sudo ti-secrets lock) and run this again, so it is mounted with $opts; nothing decrypted" ;;
    esac
  done
  case ",$have," in *,size=*) ;; *) die "$run is mounted with no size cap ($have): unmount it (sudo ti-secrets lock) and run this again; nothing decrypted" ;; esac
  chown root:root "$run"
  chmod 0700 "$run"
  if test -n "$swap"; then
    warn "swap is on (${swap% }): the secrets' tmpfs is kept out of it (noswap), but gpg, the editor and this command's memory can still be paged there; turn it off with sudo swapoff -a and remove it from /etc/fstab"
  fi
}

# new_work VAR: a fresh folder in the tmpfs, named in VAR and removed at the
# end. It is made in this shell, not a command substitution's subshell, so
# the cleanup knows it.
new_work() {
  local w
  w=$(mktemp -d "$run/.work.XXXXXX")
  works+=("$w")
  printf -v "$1" '%s' "$w"
}

# read_secret VAR PROMPT: one line, from the terminal with echo off (or the
# rehearsal's descriptor), into VAR. Never echoed, never logged.
read_secret() {
  local line=''
  if test -n "$passfd"; then
    IFS= read -r line <&"$passfd" || die "no passphrase on descriptor $passfd"
  else
    if ! { true </dev/tty; } 2>/dev/null; then
      die "no terminal to read the passphrase from: run it in an interactive shell on the server (ssh in, then sudo $prog)"
    fi
    exec 9<>/dev/tty
    printf '%s' "$2" >&9
    if ! IFS= read -r -s -u 9 line; then
      printf '\n' >&9
      die "no passphrase was read"
    fi
    printf '\n' >&9
    exec 9>&-
  fi
  printf -v "$1" '%s' "$line"
}

# read_new_passphrase VAR: a new passphrase, typed twice, at least
# $min_length characters.
read_new_passphrase() {
  local first second
  read_secret first "New passphrase (at least $min_length characters): "
  read_secret second "The same passphrase again: "
  test "$first" = "$second" || die "the two passphrases differ; nothing changed"
  test "${#first}" -ge "$min_length" || die "the passphrase is shorter than $min_length characters; nothing changed"
  printf -v "$1" '%s' "$first"
}

# run_gpg PASSPHRASE ARG...: gpg, given PASSPHRASE on a pipe (descriptor 3),
# with a throwaway home in the tmpfs, no agent, no cache, no terminal, and its
# messages in that home's err file.
gnupg_home=
run_gpg() {
  local phrase=$1
  shift
  if test -z "$gnupg_home"; then new_work gnupg_home; fi
  gpg --homedir "$gnupg_home" --no-options --no-autostart --batch --no-tty --quiet --no-symkey-cache \
    --pinentry-mode loopback --passphrase-fd 3 "$@" 2>"$gnupg_home/err" 3< <(printf '%s\n' "$phrase")
}
gpg_said() { if test -n "$gnupg_home" && test -s "$gnupg_home/err"; then tail -n 1 "$gnupg_home/err"; fi; }

# decrypt_to PASSPHRASE ENCRYPTED OUT: OUT holds the plain text, or does not
# exist and the status is 1.
decrypt_to() {
  rm -f -- "$3"
  if run_gpg "$1" --decrypt <"$2" >"$3"; then return 0; fi
  rm -f -- "$3"
  return 1
}

# encrypt_checked VAR PASSPHRASE PLAIN NAME: a temporary file of this
# command's own (mktemp, .NAME.gpg.tmp.XXXXXX beside the encrypted file),
# named in VAR (not "written", its own local), root:root 0600, synced to disk, kept only if decrypting it
# gives PLAIN back byte for byte; nothing is replaced yet (commit does that).
encrypt_checked() {
  local var=$1 phrase=$2 plain=$3 name=$4 written check
  written=$(mktemp "$(dirname "$etc/$name")/.$(basename "$name").gpg.tmp.XXXXXX")
  tmps+=("$written")
  if ! run_gpg "$phrase" "${encrypt_options[@]}" <"$plain" >"$written"; then
    die "could not encrypt $name ($(gpg_said)); nothing replaced"
  fi
  new_work check
  if ! decrypt_to "$phrase" "$written" "$check/plain" || ! cmp -s "$check/plain" "$plain"; then
    die "the encrypted $name did not decrypt to what was written: verification failed, nothing replaced and no plain text removed"
  fi
  rm -rf -- "$check"
  chown root:root "$written"
  chmod 0600 "$written"
  sync -- "$written"
  printf -v "$var" '%s' "$written"
}

# commit TMP NAME: the checked TMP renamed over NAME.gpg (one rename), and
# the folder synced, so the old or the new file is there after a crash,
# never a part of one.
commit() {
  mv -f -- "$1" "$etc/$2.gpg"
  sync -- "$(dirname "$etc/$2.gpg")"
}

# decrypt_all PASSPHRASE STAGE NAME...: every NAME decrypted into STAGE, or
# the script stops, having changed nothing.
decrypt_all() {
  local phrase=$1 stage=$2 n
  shift 2
  for n in "$@"; do
    install -d -m 0700 "$stage/$(dirname "$n")"
    decrypt_to "$phrase" "$etc/$n.gpg" "$stage/$n" ||
      die "the passphrase is wrong, or $etc/$n.gpg is damaged ($(gpg_said)); nothing changed"
  done
}

# place SOURCE NAME: SOURCE becomes the unlocked copy of NAME, in one rename
# within the tmpfs, from a temporary file of this command's own.
place() {
  local dir tmp
  dir=$(dirname "$run/$2")
  install -d -o root -g root -m 0700 "$dir"
  tmp=$(mktemp "$dir/.$(basename "$2").new.XXXXXX")
  tmps+=("$tmp")
  install -o root -g root -m 0600 "$1" "$tmp"
  mv -f -- "$tmp" "$run/$2"
}

# is_unlocked: the secrets are unlocked (an unlocked copy is in the tmpfs).
is_unlocked() {
  test -d "$run" && test -n "$(find "$run" -mindepth 1 -type f -not -name '.*' -not -path "$run/.*" -print -quit 2>/dev/null)"
}

# refuse_interrupted_change: nothing but --resume runs while a passphrase
# change is cut short, since its files are under two passphrases.
refuse_interrupted_change() {
  test ! -e "$journal" || die "a passphrase change was cut short ($journal): some files are under the old passphrase and some under the new. Finish it with sudo ti-secrets passphrase --resume (it asks for both); nothing changed"
}

refuse_plain_text() {
  local plain
  plain=$(plain_names | tr '\n' ' ')
  test -z "$plain" || die "these secret files are on disk in plain text: $plain- run sudo ti-secrets migrate first"
}

cmd_unlock() {
  local names stage n f phrase
  mapfile -t names < <(encrypted_names)
  test "${#names[@]}" -gt 0 || die "no encrypted secret file in $etc: run sudo deploy/provision.sh services"
  refuse_interrupted_change
  refuse_plain_text
  ensure_tmpfs
  read_secret phrase "Passphrase for the server's secrets: "
  new_work stage
  decrypt_all "$phrase" "$stage" "${names[@]}"
  for n in "${names[@]}"; do place "$stage/$n" "$n"; done
  # An unlocked copy whose encrypted file is gone goes too.
  while IFS= read -r -d '' f; do
    n=${f#"$run"/}
    if ! printf '%s\n' "${names[@]}" | grep -qxF -- "$n"; then rm -f -- "$f"; fi
  done < <(find "$run" -mindepth 1 -type f -not -path "$run/.*" -print0)
  log "unlocked ${#names[@]} files into $run"
  if test -d /run/systemd/system && systemctl is-enabled --quiet ti-gateway.service 2>/dev/null; then
    if systemctl is-active --quiet ti-gateway.service; then
      log "ti-gateway is already running"
    elif systemctl start --no-block ti-gateway.service; then
      log "starting ti-gateway: approve the IBKR prompt on your phone (journalctl -u ti-gateway -f)"
    else
      log "could not start ti-gateway: see journalctl -u ti-gateway"
    fi
  fi
}

cmd_lock() {
  if test -d "$run"; then
    find "$run" -mindepth 1 -delete
    if mountpoint -q "$run"; then umount "$run" 2>/dev/null || true; fi
  fi
  log "locked: no decrypted copy is left in $run; a running unit keeps the copy systemd handed it until it stops"
}

cmd_status() {
  local n state names
  mapfile -t names < <({ printf '%s\n' secrets.env ibkr-login.env; encrypted_names; plain_names; } | sort -u)
  for n in "${names[@]}"; do
    if test -f "$etc/$n"; then
      state="IN PLAIN TEXT ON DISK (run sudo ti-secrets migrate)"
    elif test -f "$etc/$n.gpg"; then
      state=encrypted
    else
      state=missing
    fi
    if test -f "$run/$n"; then state="$state, unlocked"; else state="$state, locked"; fi
    printf '%-24s %s\n' "$n" "$state"
  done
  if test -e "$journal"; then
    echo "A PASSPHRASE CHANGE WAS CUT SHORT: finish it with sudo ti-secrets passphrase --resume"
  fi
}

# set_editor_args WORK: the arguments that keep the editor's swap and
# backup files in WORK, for the editors that take them: vim (and vi, nvim)
# no swap file (-n), no viminfo, its directory and backupdir in WORK; nano no
# backup. Any other editor relies on HOME and TMPDIR being WORK.
editor_args=()
set_editor_args() {
  local first=${editor%% *} real
  real=$(readlink -f -- "$(command -v -- "$first" 2>/dev/null || printf '%s' "$first")" 2>/dev/null || printf '%s' "$first")
  case $(basename -- "$real") in
    vi | vim | vim.* | nvim | view) editor_args=(-n -i NONE --cmd "set directory=$1 backupdir=$1 undodir=$1 noundofile") ;;
    nano) editor_args=(--nobackup) ;;
    *) editor_args=() ;;
  esac
}

cmd_edit() {
  local name=$1 phrase work base editor tmp unlocked=0
  valid_name "$name" || die "'$name' is not one of the secret files (secrets.env, ibkr-login.env, accounts/<account>.env)"
  refuse_interrupted_change
  test -f "$etc/$name.gpg" || die "there is no $etc/$name.gpg (sudo ti-secrets status lists the files)"
  test ! -e "$etc/$name" || die "$etc/$name is on disk in plain text: run sudo ti-secrets migrate first"
  ensure_tmpfs
  read_secret phrase "Passphrase for the server's secrets: "
  new_work work
  base=$(basename "$name")
  decrypt_to "$phrase" "$etc/$name.gpg" "$work/.before" ||
    die "the passphrase is wrong, or $etc/$name.gpg is damaged ($(gpg_said)); nothing changed"
  install -m 0600 "$work/.before" "$work/$base"
  editor=${SUDO_EDITOR:-${VISUAL:-${EDITOR:-}}}
  if test -z "$editor"; then
    for editor in editor nano vi ''; do
      if test -n "$editor" && command -v "$editor" >/dev/null; then break; fi
    done
    test -n "$editor" || die "no editor found (set EDITOR); nothing changed"
  fi
  set_editor_args "$work"
  # The editor's home and temporary folder are in the tmpfs work folder, so
  # whatever it keeps beside the text goes with it. EDITOR may carry
  # arguments, as sudoedit and git allow. It is not handed the lock.
  # shellcheck disable=SC2086
  (cd "$work" && HOME=$work TMPDIR=$work XDG_CACHE_HOME=$work XDG_DATA_HOME=$work XDG_STATE_HOME=$work XDG_CONFIG_HOME=$work \
    $editor ${editor_args[@]+"${editor_args[@]}"} "$work/$base" {lockfd}>&-) || die "the editor exited with an error; nothing changed"
  test -f "$work/$base" || die "the editor removed the file; nothing changed"
  if is_unlocked; then unlocked=1; fi
  if cmp -s "$work/.before" "$work/$base"; then
    # An unlocked copy that differs from the encrypted file (an edit cut
    # short before it was made again) is brought up to it.
    if test "$unlocked" = 1 && ! cmp -s "$work/.before" "$run/$name"; then
      place "$work/.before" "$name"
      log "$name is unchanged; nothing written, and its unlocked copy, which was not the encrypted one, is made again from it"
    else
      log "$name is unchanged; nothing written"
    fi
    return 0
  fi
  encrypt_checked tmp "$phrase" "$work/$base" "$name"
  # The unlocked copy is never older than the encrypted file: it is removed
  # before the new encrypted file is renamed in, and made again from that
  # file. Cut short between the two, the file is missing from the tmpfs (its
  # units refuse to start) until sudo ti-unlock, or this edit run again.
  if test "$unlocked" = 1; then rm -f -- "$run/$name"; fi
  commit "$tmp" "$name"
  if test "$unlocked" = 1; then
    decrypt_to "$phrase" "$etc/$name.gpg" "$work/.after" ||
      die "$name was encrypted again, but its new encrypted file did not decrypt ($(gpg_said)); its unlocked copy is removed: run sudo ti-unlock"
    place "$work/.after" "$name"
    log "$name encrypted again and its unlocked copy made from it; each unit reads it at its next start"
  else
    log "$name encrypted again; the secrets are locked, so it is read after sudo ti-unlock"
  fi
}

# cmd_diff NAME: how the plain-text NAME and its encrypted copy differ, when
# both are on disk (a migration finds them and stops): the encrypted copy is
# decrypted into the tmpfs and compared, and the difference printed to the
# terminal, secrets included. Nothing is changed.
cmd_diff() {
  local name=$1 phrase work
  valid_name "$name" || die "'$name' is not one of the secret files (secrets.env, ibkr-login.env, accounts/<account>.env)"
  test -f "$etc/$name.gpg" || die "there is no $etc/$name.gpg (sudo ti-secrets status lists the files)"
  test -f "$etc/$name" || die "$etc/$name is not on disk in plain text: there is nothing to compare (sudo ti-secrets edit $name opens the encrypted one)"
  ensure_tmpfs
  read_secret phrase "Passphrase for the server's secrets: "
  new_work work
  decrypt_to "$phrase" "$etc/$name.gpg" "$work/encrypted" ||
    die "the passphrase is wrong, or $etc/$name.gpg is damaged ($(gpg_said)); nothing changed"
  if diff -u --label "$etc/$name.gpg (decrypted)" --label "$etc/$name (plain text)" "$work/encrypted" "$etc/$name"; then
    log "$etc/$name and $etc/$name.gpg hold the same text: sudo ti-secrets migrate removes the plain text"
  fi
}

# journal_add LINE: one line appended to the journal and synced to disk.
journal_add() {
  printf '%s\n' "$1" >>"$journal"
  sync -- "$journal"
}

# cmd_passphrase RESUME: every file encrypted again with a new passphrase.
# All the new copies are written first, each checked by decrypting it with
# the new passphrase; then all are renamed over their files. The journal
# records each step (the temporary file written for a name, then its rename),
# so a change cut short at any point is finished by --resume (RESUME=1),
# which asks for the old passphrase and the new one, opens each file with
# whichever of the two it is under, removes the temporary files the journal
# names, and does the rest.
cmd_passphrase() {
  local resume=$1 names old new stage n tmp line i todo=() tmpfor=()
  mapfile -t names < <(encrypted_names)
  test "${#names[@]}" -gt 0 || die "no encrypted secret file in $etc: run sudo deploy/provision.sh services"
  refuse_plain_text
  if test "$resume" = 1; then
    test -f "$journal" || die "no passphrase change was cut short ($journal is not there): to change the passphrase run sudo ti-secrets passphrase; nothing changed"
  else
    refuse_interrupted_change
  fi
  ensure_tmpfs
  new_work stage
  if test "$resume" = 1; then
    read_secret old "The old passphrase (before the change that was cut short): "
    read_new_passphrase new
    for n in "${names[@]}"; do
      install -d -m 0700 "$stage/$(dirname "$n")"
      if decrypt_to "$old" "$etc/$n.gpg" "$stage/$n"; then
        todo+=("$n")
      elif ! decrypt_to "$new" "$etc/$n.gpg" "$stage/$n"; then
        die "$etc/$n.gpg opens with neither passphrase ($(gpg_said)); nothing changed"
      fi
    done
    # The temporary files the cut-short change wrote and did not rename.
    while IFS= read -r line; do
      case $line in
        "written "*)
          tmp=${line#written * }
          case $tmp in
            "$etc"/.*.gpg.tmp.* | "$etc"/accounts/.*.gpg.tmp.*) rm -f -- "$tmp" ;;
          esac
          ;;
      esac
    done <"$journal"
  else
    read_secret old "Current passphrase: "
    decrypt_all "$old" "$stage" "${names[@]}"
    read_new_passphrase new
    todo=("${names[@]}")
  fi
  if test "$resume" = 0; then drop_journal=1; fi
  journal_add "change started"
  sync -- "$etc"
  # Every new copy is written and checked before any is renamed.
  for n in ${todo[@]+"${todo[@]}"}; do
    encrypt_checked tmp "$new" "$stage/$n" "$n"
    tmpfor+=("$tmp")
    journal_add "written $n $tmp"
  done
  drop_journal=0
  journal_add "renaming"
  for i in ${todo[@]+"${!todo[@]}"}; do
    commit "${tmpfor[$i]}" "${todo[$i]}"
    journal_add "renamed ${todo[$i]}"
  done
  rm -f -- "$journal"
  sync -- "$etc"
  log "the passphrase of all ${#names[@]} files is changed; the old one unlocks nothing now. Put the new one in your password manager: there is no recovery"
}

cmd_migrate() {
  local plain encrypted phrase stage n tmp
  refuse_interrupted_change
  mapfile -t plain < <(plain_names)
  if test "${#plain[@]}" = 0; then
    log "every secret file is encrypted; nothing to migrate"
    return 0
  fi
  need_gpg
  ensure_tmpfs
  mapfile -t encrypted < <(encrypted_names)
  new_work stage
  if test "${#encrypted[@]}" -gt 0; then
    read_secret phrase "Passphrase for the server's secrets: "
    decrypt_all "$phrase" "$stage" "${encrypted[@]}"
  else
    log "the secret files are kept encrypted on disk. Choose the passphrase you will type after every reboot (sudo ti-unlock), and keep it in your password manager: there is no recovery without it"
    read_new_passphrase phrase
  fi
  # A plain-text file beside its encrypted copy is the end of a migration cut
  # short when it is the same text; when it is not, which one is right is the
  # owner's to say, so nothing is changed.
  for n in "${plain[@]}"; do
    if test -f "$stage/$n" && ! cmp -s "$stage/$n" "$etc/$n"; then
      die "$etc/$n and $etc/$n.gpg both exist and differ: see how with sudo ti-secrets diff $n (it prints both to the terminal), then keep the right one: remove the plain text with sudo shred -u $etc/$n, or the encrypted copy with sudo rm $etc/$n.gpg, and run this again; nothing changed"
    fi
  done
  for n in "${plain[@]}"; do
    if ! test -f "$stage/$n"; then
      encrypt_checked tmp "$phrase" "$etc/$n" "$n"
      commit "$tmp" "$n"
    fi
    shred -u -- "$etc/$n" || die "could not shred $etc/$n; its encrypted copy is in place: remove it with sudo shred -u $etc/$n"
    test ! -e "$etc/$n" || die "$etc/$n is still there after shredding it"
    log "encrypted $n into $etc/$n.gpg, checked it decrypts to the same text, and shredded the plain text (best effort: on this storage the old blocks may survive; delete older snapshots and rotate what it held)"
  done
}

need_root
take_lock
case $prog in
  ti-unlock)
    test $# -eq 0 || usage
    need_gpg
    cmd_unlock
    ;;
  *)
    case ${1:-} in
      unlock) test $# -eq 1 || usage; need_gpg; cmd_unlock ;;
      lock) test $# -eq 1 || usage; cmd_lock ;;
      status) test $# -eq 1 || usage; cmd_status ;;
      migrate) test $# -eq 1 || usage; cmd_migrate ;;
      passphrase)
        if test $# -eq 1; then
          need_gpg
          cmd_passphrase 0
        elif test $# -eq 2 && test "$2" = --resume; then
          need_gpg
          cmd_passphrase 1
        else
          usage
        fi
        ;;
      edit) test $# -eq 2 || usage; need_gpg; cmd_edit "$2" ;;
      diff) test $# -eq 2 || usage; need_gpg; cmd_diff "$2" ;;
      *) usage ;;
    esac
    ;;
esac
