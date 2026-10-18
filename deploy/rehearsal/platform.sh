#!/bin/bash
# Sourced by deploy/rehearse.sh. One platform is pinned for the container and
# for the Go tools built to run in it, so they cannot disagree.

# The container's platform: TI_REHEARSAL_PLATFORM, else DOCKER_DEFAULT_PLATFORM
# (which Docker would apply to the container anyway), else the daemon's own.
rehearsal_platform() {
  if test -n "${TI_REHEARSAL_PLATFORM:-}"; then
    echo "$TI_REHEARSAL_PLATFORM"
  elif test -n "${DOCKER_DEFAULT_PLATFORM:-}"; then
    echo "$DOCKER_DEFAULT_PLATFORM"
  else
    echo "linux/$(docker_arch)"
  fi
}

docker_arch() { docker info --format '{{.Architecture}}'; }

# GOARCH for a platform (linux/amd64, linux/arm64/v8, linux/x86_64 ...).
rehearsal_goarch() {
  case $1 in
    */amd64 | */amd64/* | */x86_64) echo amd64 ;;
    */arm64 | */arm64/* | */aarch64) echo arm64 ;;
    *) echo "rehearse: unsupported container platform '$1'" >&2; return 1 ;;
  esac
}

# Succeeds when the tools' GOARCH ($1) is the architecture of the container
# ($2: a platform such as linux/amd64, or a bare architecture such as arm64).
rehearsal_arch_matches() {
  local actual=$2
  case $actual in */*) ;; *) actual=linux/$actual ;; esac
  actual=$(rehearsal_goarch "$actual") || return 1
  if test "$1" != "$actual"; then
    echo "rehearse: the tools are built for $1 but the container is $actual; refusing to run" >&2
    return 1
  fi
}
