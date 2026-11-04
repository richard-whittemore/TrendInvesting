#!/bin/bash
# Stages only the selected login in the Gateway's runtime tmpfs (ADR 0028;
# docs/runbooks/gateway-login.md). systemd runs this as root with ExecStartPre=+.
# Never trace or print secret contents. Source and destination opens refuse links.
# TI_GATEWAY_LOGIN_FILE and TI_GATEWAY_SECRETS_DIRECTORY override paths for rehearsal.
set -euo pipefail
set +x
umask 077
exec python3 -I - <<'PY'
import os
from pathlib import Path
import pwd
import stat
import sys


def refuse(message):
    print(f"stage-gateway-login: {message}", file=sys.stderr)
    sys.exit(64)


runtime = os.open(os.environ["RUNTIME_DIRECTORY"], os.O_RDONLY | os.O_DIRECTORY | os.O_NOFOLLOW)
# A refused start must leave no previous login available. unlink never follows it.
try:
    os.unlink("login.env", dir_fd=runtime)
except FileNotFoundError:
    pass

selector = os.environ.get("TI_GATEWAY_LOGIN_FILE", "/etc/trend-investing/gateway-login")
instance = os.environ.get("TI_GATEWAY_INSTANCE", "")
if instance:
    if instance not in ("trad-ira", "roth-ira"):
        refuse("unknown Gateway instance")
    selection = instance
    selector = "Gateway instance"
else:
    try:
        raw = Path(selector).read_bytes()
    except OSError:
        refuse(f"missing or unreadable selector {selector}; run sudo deploy/provision.sh services")
    selection = raw.removesuffix(b"\n")
    if selection not in (b"joint", b"trad-ira", b"roth-ira", b"live"):
        refuse(f"{selector} must contain one word: joint | trad-ira | roth-ira | live")
    selection = selection.decode("ascii")
login_name = f"ibkr-login-{selection}.env"
secrets = os.environ.get("TI_GATEWAY_SECRETS_DIRECTORY", "/run/trend-investing/secrets")
try:
    source_dir = os.open(secrets, os.O_RDONLY | os.O_DIRECTORY | os.O_NOFOLLOW)
    try:
        source = os.open(login_name, os.O_RDONLY | os.O_NOFOLLOW | os.O_NONBLOCK, dir_fd=source_dir)
    finally:
        os.close(source_dir)
    with os.fdopen(source, "rb") as login:
        info = os.fstat(login.fileno())
        if not stat.S_ISREG(info.st_mode) or info.st_size == 0:
            raise ValueError("not a nonempty regular file")
        # Match the renderer's literal KEY=value parsing, including last-key wins
        # (docs/runbooks/gateway-login.md). Never source the owner's secret.
        content = login.read()
        fields = dict(line.split(b"=", 1) for line in content.split(b"\n")
                      if b"=" in line and not line.startswith(b"#"))
        expected_mode = b"live" if selection == "live" else b"paper"
        if b"\x00" in content or fields.get(b"TWS_TRADING_MODE") != expected_mode:
            refuse(f"{login_name} selected by {selector} ({selection}) requires "
                   f"TWS_TRADING_MODE={expected_mode.decode()}; correct it with sudo ti-secrets edit {login_name}")
        if selection == "live" and b"TWS_READONLY_API" not in fields:
            refuse(f"{login_name} requires explicit TWS_READONLY_API in live mode")
        owner = pwd.getpwnam("ti-gateway")
        destination = os.open("login.env", os.O_WRONLY | os.O_CREAT | os.O_EXCL | os.O_NOFOLLOW,
                              0o600, dir_fd=runtime)
        with os.fdopen(destination, "wb") as staged:
            staged.write(content)
            staged.flush()
            if os.fstat(staged.fileno()).st_size == 0:
                raise ValueError("empty copy")
            os.fchmod(staged.fileno(), 0o600)
            os.fchown(staged.fileno(), owner.pw_uid, owner.pw_gid)
except (OSError, ValueError, KeyError):
    try:
        os.unlink("login.env", dir_fd=runtime)
    except FileNotFoundError:
        pass
    refuse(f"missing, empty, linked or unreadable {login_name} selected by {selector} ({selection}); "
           f"sudo ti-secrets edit {login_name}, then sudo ti-unlock")
finally:
    os.close(runtime)
PY
