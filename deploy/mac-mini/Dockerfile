# The Mac mini's rehearsal container image: Ubuntu 24.04 for arm64 with
# systemd as PID 1, and nothing of the program. deploy/mac-mini/up.sh builds
# it from this file alone (no build context, so nothing is read from the
# checkout) and then runs deploy/provision.sh inside, as on the Lightsail
# server (docs/manual/mac-mini.md).
#
# The base is pinned by digest (the multi-platform index of ubuntu:24.04 of
# 2026-09-30); up.sh asks for its linux/arm64 image. The packages below are
# Ubuntu's current ones when the image is built; the provisioner's own
# downloads stay pinned by checksum in deploy/versions.env.
FROM ubuntu:24.04@sha256:68ee22004a9109b44973313b02ff7b1de250f1de0f5ecb628d73f579aaff33ac

ENV container=docker

RUN apt-get update -qq \
 && DEBIAN_FRONTEND=noninteractive apt-get install -y -qq --no-install-recommends \
      systemd systemd-sysv dbus ca-certificates curl python3 tzdata >/dev/null \
 && rm -rf /var/lib/apt/lists/* \
 # The machine ID is left empty so that systemd takes it from container_uuid,
 # which up.sh derives from the container's name: the journal files in
 # /var/log/journal/<machine ID>/ are then still this machine's after the
 # container is removed and created again.
 && : >/etc/machine-id \
 && rm -f /var/lib/dbus/machine-id \
 && ln -s /etc/machine-id /var/lib/dbus/machine-id \
 # Nothing in a container needs a login prompt.
 && systemctl mask getty.target console-getty.service systemd-firstboot.service \
 # binfmt_misc belongs to Docker's Linux VM and is shared by every container:
 # systemd-binfmt clears it at boot and registers Ubuntu's own handlers, which
 # removes Docker Desktop's Rosetta handler, so that no amd64 container runs
 # on the Mac until Docker Desktop restarts. The container never touches it.
 && systemctl mask systemd-binfmt.service proc-sys-fs-binfmt_misc.automount proc-sys-fs-binfmt_misc.mount

# Docker starts the container with every mount's propagation private; systemd
# on a machine of its own makes it shared at boot, and its credentials
# (LoadCredential, which hands the reporter, the data job and the engines
# their secrets) depend on it: a credential mounted by systemd's helper never
# reaches /run/credentials otherwise. This unit does what that boot step
# would, before anything else starts.
RUN printf '%s\n' \
      '[Unit]' \
      'Description=Shared mount propagation, as on a machine of its own (for systemd credentials)' \
      'DefaultDependencies=no' \
      'Before=sysinit.target local-fs.target' \
      '' \
      '[Service]' \
      'Type=oneshot' \
      'RemainAfterExit=yes' \
      'ExecStart=/usr/bin/mount --make-rshared /' \
      '' \
      '[Install]' \
      'WantedBy=sysinit.target' >/etc/systemd/system/mount-shared.service \
 && systemctl enable mount-shared.service

# systemd shuts down cleanly on this signal (docker stop and docker restart).
STOPSIGNAL SIGRTMIN+3
# up.sh runs the container without --privileged, with CAP_SYS_ADMIN and a
# cgroup namespace of its own, in which Docker mounts the cgroup tree
# read-only; systemd cannot start without writing to it. The tree is the
# container's own (the namespace hides the rest of the VM's), so it is made
# writable here before systemd starts, at every start of the container.
ENTRYPOINT ["/bin/sh", "-c", "mount -o remount,rw /sys/fs/cgroup && exec /sbin/init"]
