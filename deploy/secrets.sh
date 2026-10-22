#!/bin/bash
# ti-secrets and ti-unlock: the server's secret files rest on disk only
# encrypted, with a passphrase the owner chooses and types; after each boot
# the owner unlocks them into a root-only tmpfs, which the units load their
# credentials from (owner decision 2026-10-22). Lightsail offers no TPM, so a
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
#   sudo ti-secrets passphrase     change the passphrase of every file at once
#   sudo ti-secrets lock           remove the decrypted copies (a running unit
#                                  keeps the copy systemd handed it)
#   sudo ti-secrets status         which files are encrypted and which unlocked;
#                                  asks for nothing, prints no secret
#   sudo ti-secrets migrate        encrypt every secret file still in plain text
#                                  (provision.sh services runs it): each is
#                                  encrypted, checked by decrypting it, then
#                                  shredded and removed; a check that fails
#                                  stops it with the plain text kept
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
# backup and history files are removed with the plain text. Do not run this
# with `bash -x`.
#
# TI_PASSPHRASE_FD=N reads the passphrase lines from descriptor N instead of
# the terminal: the rehearsal's only, honoured where TI_REHEARSAL=1 and
# refused anywhere else.
set -euo pipefail
set +x
umask 077

etc=/etc/trend-investing
run=/run/trend-investing/secrets
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

usage() {
  die "usage: ti-unlock, or ti-secrets unlock|lock|status|migrate|passphrase, or ti-secrets edit NAME (secrets.env, ibkr-login.env, accounts/<account>.env)"
}

passfd=
if test -n "${TI_PASSPHRASE_FD:-}"; then
  test "${TI_REHEARSAL:-0}" = 1 || die "TI_PASSPHRASE_FD is the rehearsal's only (TI_REHEARSAL=1); on the server the passphrase is typed at the terminal"
  case $TI_PASSPHRASE_FD in '' | *[!0-9]*) die "TI_PASSPHRASE_FD must be a descriptor number" ;; esac
  passfd=$TI_PASSPHRASE_FD
fi

# Folders made in the tmpfs for one run (the staging of decrypted files, gpg's
# home, an edit), and temporary encrypted files beside the real ones: all
# removed when the script ends, however it ends.
works=()
tmps=()
cleanup() {
  local f
  for f in ${tmps[@]+"${tmps[@]}"}; do rm -f -- "$f"; done
  for f in ${works[@]+"${works[@]}"}; do rm -rf -- "$f"; done
}
trap cleanup EXIT

need_root() { test "$(id -u)" -eq 0 || die "run as root (sudo $prog ...)"; }

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

# ensure_tmpfs: $run, a memory file system of its own, root:root 0700. On
# the server it is mounted here; a rehearsal container that cannot mount is
# given one. Anything else is refused, so nothing is decrypted onto a disk.
ensure_tmpfs() {
  test ! -L "$run" || die "$run is a link; nothing decrypted"
  install -d -o root -g root -m 0755 /run/trend-investing
  if ! test -d "$run"; then install -d -o root -g root -m 0700 "$run"; fi
  if ! mountpoint -q "$run"; then
    mount -t tmpfs -o mode=0700,uid=0,gid=0,nosuid,nodev,noexec,size=4m ti-secrets "$run" 2>/dev/null ||
      die "cannot mount a tmpfs at $run (the decrypted secrets must stay in memory, never on disk); nothing decrypted"
  fi
  case $(stat -f -c %T "$run") in
    tmpfs | ramfs) ;;
    *) die "$run is not a memory file system ($(stat -f -c %T "$run")); nothing decrypted" ;;
  esac
  chown root:root "$run"
  chmod 0700 "$run"
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

# encrypt_checked PASSPHRASE PLAIN NAME: NAME.gpg.tmp beside the encrypted
# file, root:root 0600, synced to disk, written only if decrypting it gives
# PLAIN back byte for byte; nothing is replaced yet (commit does that).
encrypt_checked() {
  local phrase=$1 plain=$2 name=$3 tmp="$etc/$3.gpg.tmp" check
  rm -f -- "$tmp"
  tmps+=("$tmp")
  if ! run_gpg "$phrase" "${encrypt_options[@]}" <"$plain" >"$tmp"; then
    die "could not encrypt $name ($(gpg_said)); nothing replaced"
  fi
  new_work check
  if ! decrypt_to "$phrase" "$tmp" "$check/plain" || ! cmp -s "$check/plain" "$plain"; then
    die "the encrypted $name did not decrypt to what was written: verification failed, nothing replaced and no plain text removed"
  fi
  rm -rf -- "$check"
  chown root:root "$tmp"
  chmod 0600 "$tmp"
  sync -- "$tmp"
}

# commit NAME: the checked NAME.gpg.tmp renamed over NAME.gpg (one rename),
# and the folder synced, so the old or the new file is there after a crash,
# never a part of one.
commit() {
  local tmp="$etc/$1.gpg.tmp"
  mv -f -- "$tmp" "$etc/$1.gpg"
  sync -- "$(dirname "$etc/$1.gpg")"
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
# within the tmpfs.
place() {
  install -d -o root -g root -m 0700 "$run/$(dirname "$2")"
  install -o root -g root -m 0600 "$1" "$run/$2.new"
  mv -f -- "$run/$2.new" "$run/$2"
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
}

cmd_edit() {
  local name=$1 phrase work base editor
  valid_name "$name" || die "'$name' is not one of the secret files (secrets.env, ibkr-login.env, accounts/<account>.env)"
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
  # The editor's home and temporary folder are in the tmpfs work folder, so
  # whatever it keeps beside the text goes with it. EDITOR may carry
  # arguments, as sudoedit and git allow.
  # shellcheck disable=SC2086
  (cd "$work" && HOME=$work TMPDIR=$work XDG_CACHE_HOME=$work XDG_DATA_HOME=$work XDG_STATE_HOME=$work XDG_CONFIG_HOME=$work \
    $editor "$work/$base") || die "the editor exited with an error; nothing changed"
  test -f "$work/$base" || die "the editor removed the file; nothing changed"
  if cmp -s "$work/.before" "$work/$base"; then
    log "$name is unchanged; nothing written"
    return 0
  fi
  encrypt_checked "$phrase" "$work/$base" "$name"
  commit "$name"
  if test -f "$run/$name"; then
    place "$work/$base" "$name"
    log "$name encrypted again and its unlocked copy replaced; each unit reads it at its next start"
  else
    log "$name encrypted again; the secrets are locked, so it is read after sudo ti-unlock"
  fi
}

cmd_passphrase() {
  local names old new stage n
  mapfile -t names < <(encrypted_names)
  test "${#names[@]}" -gt 0 || die "no encrypted secret file in $etc: run sudo deploy/provision.sh services"
  refuse_plain_text
  ensure_tmpfs
  read_secret old "Current passphrase: "
  new_work stage
  decrypt_all "$old" "$stage" "${names[@]}"
  read_new_passphrase new
  # Every file is encrypted and checked before any is replaced; then each is
  # replaced in one rename.
  for n in "${names[@]}"; do encrypt_checked "$new" "$stage/$n" "$n"; done
  for n in "${names[@]}"; do commit "$n"; done
  log "the passphrase of all ${#names[@]} files is changed; the old one unlocks nothing now. Put the new one in your password manager: there is no recovery"
}

cmd_migrate() {
  local plain encrypted phrase stage n
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
      die "$etc/$n and $etc/$n.gpg both exist and differ: keep the right one (sudo ti-secrets edit $n shows the encrypted one; remove the plain text with sudo shred -u $etc/$n, or the encrypted copy with sudo rm $etc/$n.gpg), then run this again; nothing changed"
    fi
  done
  for n in "${plain[@]}"; do
    if ! test -f "$stage/$n"; then
      encrypt_checked "$phrase" "$etc/$n" "$n"
      commit "$n"
    fi
    shred -u -- "$etc/$n" || die "could not shred $etc/$n; its encrypted copy is in place: remove it with sudo shred -u $etc/$n"
    test ! -e "$etc/$n" || die "$etc/$n is still there after shredding it"
    log "encrypted $n into $etc/$n.gpg, checked it decrypts to the same text, and shredded the plain text"
  done
}

need_root
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
      passphrase) test $# -eq 1 || usage; need_gpg; cmd_passphrase ;;
      edit) test $# -eq 2 || usage; need_gpg; cmd_edit "$2" ;;
      *) usage ;;
    esac
    ;;
esac
