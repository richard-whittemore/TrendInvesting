#!/bin/bash
# Checks the server's loopback-only Gateway guard (docs/manual/server.md,
# "The server's own guard on the Gateway's ports") without changing the host.
set -euo pipefail
here=$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)
work=$(mktemp -d)
trap 'rm -rf "$work"' EXIT
rules="$here/../etc/nftables/ti-gateway-guard.nft"

python3 - "$here/.." <<'PY'
import pathlib
import re
import sys

deploy = pathlib.Path(sys.argv[1]).resolve()
rules = deploy / "etc/nftables/ti-gateway-guard.nft"
assert rules.is_file(), "missing Gateway guard ruleset"
text = re.sub(r"#[^\n]*", "", rules.read_text())
assert re.search(r'table inet ti_gateway_guard\s*\{\s*\}\s*;?\s*delete table inet ti_gateway_guard\s*;?\s*table inet ti_gateway_guard\s*\{', text), "missing ordered declare/delete/define idiom"
assert re.search(r'type filter hook input priority -[1-9][0-9]*\s*;', text), "input must run before filter priority 0"
assert re.search(r'tcp dport 4001-4004 iifname != "lo" drop\s*;', text), "exact TCP ports, loopback exception and drop required"
assert text.count("drop") == 1 and text.count("tcp dport") == 1
assert "flush ruleset" not in text, "must preserve other firewall tables"
unit = (deploy / "systemd/ti-gateway-guard.service").read_text()
for line in ("Type=oneshot", "RemainAfterExit=yes", "After=network-pre.target",
             "WantedBy=multi-user.target", "OnFailure=ti-unit-failed@%n.service",
             "ExecStart=/usr/sbin/nft -f /etc/nftables/ti-gateway-guard.nft"):
    assert line in unit.splitlines(), line
assert not any(line.startswith("Before=") and "@.service" in line for line in unit.splitlines()), "template is not an ordering target"
assert "ExecStop=" not in unit, "stopping the unit must not remove the guard"
for name in ("ti-gateway.service", "ti-gateway@.service"):
    gateway = (deploy / "systemd" / name).read_text()
    for key in ("Requires", "After"):
        assert any(line.startswith(key + "=") and "ti-gateway-guard.service" in line.split("=", 1)[1].split()
                   for line in gateway.splitlines()), (name, key)
manual = (deploy.parent / "docs/manual/server.md").read_text()
assert "sudo systemctl restart ti-gateway-guard" not in manual
assert "sudo nft -f /etc/nftables/ti-gateway-guard.nft" in manual
install = manual.split("**Installing the guard on a running server.**", 1)[1].split("As the owner, check:", 1)[0]
install = " ".join(install.split())
for text in ("`sudo deploy/provision.sh services` from a checkout of the merged commit",
             "[Installing a build](#installing-a-build)",
             "installs and loads the guard before installing the Gateway units",
             "Both Gateways keep running and take the new dependency at their next start",
             "Optionally run `sudo deploy/provision.sh firewall` to re-apply ufw"):
    assert text in install, text
owner = manual.split("As the owner, check:", 1)[1].split("## Closing the Gateway", 1)[0]
assert "tailscale ip -4" not in owner and "/dev/tcp/" not in owner
assert "`nc -vz -G 5 trend-investing-1 4002`" in owner
assert "must time out" in " ".join(owner.split())
assert "also blocked by the Tailscale policy" in " ".join(owner.split())
assert "nft listing shows the server's own guard" in " ".join(owner.split())
assert "never by restarting the unit" in " ".join(manual.split())
reboot = manual.split("## After a reboot", 1)[1].split("## Reading the logs", 1)[0]
for text in ("systemctl status ti-gateway-guard --no-pager",
             "ti-gateway-guard.service failed",
             "sudo nft -f /etc/nftables/ti-gateway-guard.nft",
             "sudo systemctl start ti-gateway-guard", "sudo systemctl start ti-gateway"):
    assert text in reboot, text
paper = (deploy.parent / "docs/runbooks/first-paper-order.md").read_text()
a4 = paper.split("### A4.", 1)[1].split("### A5.", 1)[0]
assert "tailscale ip -4" not in a4 and "/dev/tcp/" not in a4
assert "`nc -vz -G 5 trend-investing-1 4002`" in a4
assert "also blocked by the Tailscale policy" in " ".join(a4.split())
rehearsal = (deploy / "rehearsal/inside-systemd.sh").read_text()
assert '/etc/systemd/system/ti-gateway-guard.service.d' in rehearsal and 'ExecStart=/bin/true' in rehearsal, "systemd rehearsal needs a guard stand-in without kernel firewall access"
PY

# Extract the firewall step and shared guard helpers; every mutating command is a recording stub.
helpers=$(awk '/^gateway_guard_[a-z_]+\(\)/,/^}/' "$here/../provision.sh")
step=$(awk '/^step_firewall\(\)/,/^}/' "$here/../provision.sh")
run() {
  bash -eu -c '
    here=$1 unit_dir=/etc/systemd/system rehearsal=$2 target=$3 missing=$4
    log() { echo "$*"; }
    die() { echo "$*" >&2; exit 1; }
    mac_mini() { test "$target" = mac-mini; }
    command() { if test "$*" = "-v nft"; then test "$missing" = 0; else builtin command "$@"; fi; }
    ip() { :; }
    tailscale() { echo 100.64.0.1; }
    ufw() { echo "ufw $*" >>"$TEST_CALLS"; }
    nft() { echo "nft $*" >>"$TEST_CALLS"; test "${FAIL_NFT:-0}" != 1; }
    install() { echo "install $*" >>"$TEST_CALLS"; }
    systemctl() { echo "systemctl $*" >>"$TEST_CALLS"; test "${FAIL_GUARD:-0}" != 1; }
    eval "$5"
    step_firewall
  ' _ "$here/.." "$1" "$2" "$3" "$helpers
$step" >"$work/out" 2>&1
}
export TEST_CALLS="$work/calls"
: >"$TEST_CALLS"
run 0 lightsail 0 || { cat "$work/out"; exit 1; }
grep -q '^install -m 0644 .*etc/nftables/ti-gateway-guard.nft /etc/nftables/' "$TEST_CALLS"
grep -q '^install -m 0644 .*systemd/ti-gateway-guard.service /etc/systemd/system/' "$TEST_CALLS"
grep -qx 'systemctl daemon-reload' "$TEST_CALLS"
grep -qx 'systemctl enable ti-gateway-guard.service' "$TEST_CALLS"
python3 - "$TEST_CALLS" <<'PY'
import pathlib, sys
lines = pathlib.Path(sys.argv[1]).read_text().splitlines()
expected = ["ufw --force enable", "systemctl daemon-reload",
            "systemctl enable ti-gateway-guard.service",
            "nft -f /etc/nftables/ti-gateway-guard.nft",
            "systemctl start ti-gateway-guard.service"]
positions = [lines.index(call) for call in expected]
assert positions == sorted(positions), lines
assert not any("restart" in line or "--now ti-gateway-guard" in line for line in lines), lines
PY
# Reprovisioning must load the table even when the oneshot is already active.
: >"$TEST_CALLS"
run 0 lightsail 0
grep -qx 'nft -f /etc/nftables/ti-gateway-guard.nft' "$TEST_CALLS"
: >"$TEST_CALLS"
if FAIL_NFT=1 run 0 lightsail 0; then echo 'FAIL: ruleset load failure accepted'; exit 1; fi
if grep -qx 'systemctl start ti-gateway-guard.service' "$TEST_CALLS"; then echo 'FAIL: started guard after load failure'; exit 1; fi
: >"$TEST_CALLS"
if run 0 lightsail 1; then echo 'FAIL: missing nft accepted'; exit 1; fi
grep -qi 'nft.*required' "$work/out"
test ! -s "$TEST_CALLS"
: >"$TEST_CALLS"
if FAIL_GUARD=1 run 0 lightsail 0; then echo 'FAIL: guard service failure accepted'; exit 1; fi
for scenario in '1 lightsail' '0 mac-mini'; do
  : >"$TEST_CALLS"
  # shellcheck disable=SC2086
  run $scenario 1
  test ! -s "$TEST_CALLS"
done

if command -v nft >/dev/null 2>&1; then
  if nft -c -f "$rules" >"$work/nft.out" 2>&1; then
    nft -c -f "$rules"
    echo 'nft check-only validation passed twice (no rules loaded)'
  elif grep -Eqi 'Operation not permitted|Permission denied' "$work/nft.out"; then
    echo 'SKIP: live nft check needs kernel permissions; idempotence checked statically'
  else
    cat "$work/nft.out" >&2
    exit 1
  fi
else
  echo 'SKIP: nft unavailable; idempotence checked statically'
fi
echo 'gateway guard tests ok'
