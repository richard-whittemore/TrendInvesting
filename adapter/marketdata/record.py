"""Recording layer: a fetched input becomes one immutable file the engines
read, so a replay never touches the network.

The file holds the record and a SHA-256 of its canonical JSON. A recording is
never overwritten (a replay must read what the live run read). The
`market.universe.snapshot` envelope encoding is a separate layer that reads
these files; it is not built here.

Installation follows ADR 0018: the hard link is the commit point, and the
name is durable only once its directory is synced, so after the link the
destination directory and the parent of every directory created to reach it
are synced, innermost first. A sync that fails after the link leaves the
file recorded and says so (NotDurable): re-recording would be refused.
"""
import hashlib
import json
import os
import tempfile


class AlreadyRecorded(Exception):
    pass


class Corrupt(Exception):
    pass


class NotDurable(Exception):
    """The recording is installed at ``path``, but a directory sync failed."""

    def __init__(self, path, err):
        super().__init__("{} is recorded, but syncing its directory failed: {}; confirm it reads, "
                         "do not record it again".format(path, err))
        self.path = path


def _sync_dir(path):
    if os.name == "nt":   # directories cannot be synced on Windows
        return
    fd = os.open(path, os.O_RDONLY)
    try:
        os.fsync(fd)
    finally:
        os.close(fd)


def _missing_dirs(directory):
    """The directories, innermost first, that do not exist yet on the way to ``directory``."""
    out, d = [], os.path.abspath(directory)
    while not os.path.isdir(d):
        out.append(d)
        parent = os.path.dirname(d)
        if parent == d:
            break
        d = parent
    return out


def _canonical(record):
    return json.dumps(record, sort_keys=True, separators=(",", ":"))


def filename(record):
    """kind_session.json; a record covering a window (``from``) is named
    kind_from_session.json."""
    if record.get("from"):
        return "{}_{}_{}.json".format(record["kind"], record["from"], record["session"])
    return "{}_{}.json".format(record["kind"], record["session"])


def write(directory, record):
    """Writes ``record`` atomically and returns the path. Raises
    AlreadyRecorded if the file exists; leaves nothing behind on failure."""
    canonical = _canonical(record)   # fails before any file exists
    doc = json.dumps({"sha256": hashlib.sha256(canonical.encode()).hexdigest(), "record": record},
                     sort_keys=True)
    created = _missing_dirs(directory)
    os.makedirs(directory, exist_ok=True)
    final = os.path.join(directory, filename(record))
    fd, tmp = tempfile.mkstemp(dir=directory, suffix=".part")
    try:
        with os.fdopen(fd, "w") as f:
            f.write(doc)
            f.flush()
            os.fsync(f.fileno())
        try:
            os.link(tmp, final)   # fails if the recording exists
        except FileExistsError:
            raise AlreadyRecorded(final) from None
    finally:
        os.remove(tmp)
    try:
        for d in [os.path.abspath(directory)] + [os.path.dirname(c) for c in created]:
            _sync_dir(d)
    except OSError as e:
        raise NotDurable(final, e) from None
    return final


def read(path):
    with open(path) as f:
        return read_open(f, path)


def read_open(f, path):
    """The record in the open file ``f`` (named ``path`` for errors), checked
    against its digest as read does."""
    doc = json.load(f)
    digest = hashlib.sha256(_canonical(doc["record"]).encode()).hexdigest()
    if digest != doc.get("sha256"):
        raise Corrupt(path)
    return doc["record"]
