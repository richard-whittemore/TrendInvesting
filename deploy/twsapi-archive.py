"""Unpacks the Python client from the pinned TWS API Mac/Unix archive: provision.sh twsapi's arm64 path.

    python3 -I deploy/twsapi-archive.py ARCHIVE SHA256 DEST

The x86_64-only installer cannot run on arm64, so on arm64 the step
installs from the same release's Mac/Unix zip, pinned beside the installer
in deploy/versions.env (TWSAPI_ARCHIVE_URL, TWSAPI_ARCHIVE_SHA256; owner
decision 2026-10-18, under ADR 0025 point 4's fetched-and-verified rule).
ARCHIVE's SHA-256 must equal SHA256 before anything is read from it. Then
its one source/pythonclient tree, which must hold a setup.py or a
pyproject.toml, is unpacked as DEST/pythonclient, files 0644 and
directories 0755, and nothing else of the archive. An archive with any
entry whose name is not a plain relative path (absolute, or with a ".."
part, which could land outside DEST), or a pythonclient entry that is a
symbolic link, is refused. DEST must not exist; on any refusal nothing is
left there (fail closed: the step installs nothing). Standard library only.
"""
import hashlib
import os
import re
import shutil
import stat
import sys
import tempfile
import zipfile

_SHA256 = re.compile(r"[0-9a-f]{64}")
_CLIENT = re.compile(r"((?:[^/]+/)*source/pythonclient/)")


class Refused(Exception):
    """The archive is not the pinned one, or not laid out as the pinned release is; nothing is unpacked."""


def _sha256(path):
    digest = hashlib.sha256()
    with open(path, "rb") as f:
        for block in iter(lambda: f.read(1 << 20), b""):
            digest.update(block)
    return digest.hexdigest()


def _client_entries(archive):
    """The pythonclient prefix and its entries ((relative name, ZipInfo), ...), checked, else Refused."""
    prefixes = set()
    for info in archive.infolist():
        parts = info.filename.rstrip("/").split("/")
        if info.filename.startswith("/") or "\\" in info.filename or any(p in ("", ".", "..") for p in parts):
            raise Refused("the archive entry {!r} is not a plain relative name".format(info.filename))
        match = _CLIENT.match(info.filename)
        if match:
            prefixes.add(match.group(1))
    if len(prefixes) != 1:
        raise Refused("the archive has not exactly one source/pythonclient ({} found)".format(len(prefixes)))
    prefix = prefixes.pop()
    entries = []
    for info in archive.infolist():
        if not info.filename.startswith(prefix):
            continue
        name = info.filename[len(prefix):]
        if not name:
            continue
        if stat.S_ISLNK(info.external_attr >> 16):
            raise Refused("the archive entry {!r} is a symbolic link".format(info.filename))
        entries.append((name, info))
    if not any(name in ("setup.py", "pyproject.toml") for name, _ in entries):
        raise Refused("the archive's pythonclient has no setup.py or pyproject.toml")
    return entries


def unpack(archive_path, sha256, dest):
    """Unpack the verified archive's pythonclient as dest/pythonclient, else raise Refused with nothing at dest."""
    if not _SHA256.fullmatch(sha256):
        raise Refused("the pinned SHA-256 {!r} is not in the form the pin takes".format(sha256))
    if os.path.lexists(dest):
        raise Refused("the destination {} already exists".format(dest))
    actual = _sha256(archive_path)
    if actual != sha256:
        raise Refused("checksum mismatch for {} (expected {}, got {}); not installing".format(
            archive_path, sha256, actual))
    try:
        archive = zipfile.ZipFile(archive_path)
    except zipfile.BadZipFile as err:
        raise Refused("{} is not a zip archive: {}".format(archive_path, err)) from None
    parent = os.path.dirname(os.path.abspath(dest))
    scratch = tempfile.mkdtemp(prefix=".twsapi-", dir=parent)
    try:
        with archive:
            entries = _client_entries(archive)
            root = os.path.join(scratch, "pythonclient")
            os.mkdir(root, 0o755)
            for name, info in entries:
                target = os.path.join(root, *name.rstrip("/").split("/"))
                if info.is_dir():
                    os.makedirs(target, 0o755, exist_ok=True)
                    continue
                os.makedirs(os.path.dirname(target), 0o755, exist_ok=True)
                with archive.open(info) as src, open(target, "xb") as out:
                    shutil.copyfileobj(src, out)
        for here, dirs, files in os.walk(scratch):
            os.chmod(here, 0o755)
            for f in files:
                os.chmod(os.path.join(here, f), 0o644)
        os.rename(scratch, dest)
    except BaseException:
        shutil.rmtree(scratch, ignore_errors=True)
        raise


def main(argv):
    if len(argv) != 3:
        print("usage: twsapi-archive.py ARCHIVE SHA256 DEST", file=sys.stderr)
        return 2
    try:
        unpack(*argv)
    except Refused as refusal:
        print("twsapi-archive: {}".format(refusal), file=sys.stderr)
        return 1
    return 0


if __name__ == "__main__":
    sys.exit(main(sys.argv[1:]))
