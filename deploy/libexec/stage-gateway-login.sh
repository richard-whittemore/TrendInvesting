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
import shutil
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
        owner = pwd.getpwnam("ti-gateway")
        destination = os.open("login.env", os.O_WRONLY | os.O_CREAT | os.O_EXCL | os.O_NOFOLLOW,
                              0o600, dir_fd=runtime)
        with os.fdopen(destination, "wb") as staged:
            shutil.copyfileobj(login, staged)
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
