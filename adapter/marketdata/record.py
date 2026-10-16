"""Recording layer: a fetched input becomes one immutable file the engines
read, so a replay never touches the network.

The file holds the record and a SHA-256 of its canonical JSON. A recording is
never overwritten (a replay must read what the live run read). The
`market.universe.snapshot` envelope encoding is a separate layer that reads
these files; it is not built here.
"""
import hashlib
import json
import os
import tempfile


class AlreadyRecorded(Exception):
    pass


class Corrupt(Exception):
    pass


def _canonical(record):
    return json.dumps(record, sort_keys=True, separators=(",", ":"))


def filename(record):
    return "{}_{}.json".format(record["kind"], record["session"])


def write(directory, record):
    """Writes ``record`` atomically and returns the path. Raises
    AlreadyRecorded if the file exists; leaves nothing behind on failure."""
    canonical = _canonical(record)   # fails before any file exists
    doc = json.dumps({"sha256": hashlib.sha256(canonical.encode()).hexdigest(), "record": record},
                     sort_keys=True)
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
    return final


def read(path):
    with open(path) as f:
        doc = json.load(f)
    digest = hashlib.sha256(_canonical(doc["record"]).encode()).hexdigest()
    if digest != doc.get("sha256"):
        raise Corrupt(path)
    return doc["record"]
