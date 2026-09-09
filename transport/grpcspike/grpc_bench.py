"""Python gRPC half of the rejected alternative in the transport spike.

Lives outside the repository alongside grpcprobe/main.go, for the same reason:
measuring gRPC must not commit the project to it.

There is no generated stub. The method carries a protobuf BytesValue holding
exactly the envelope JSON the Unix-socket candidate sends, and BytesValue's
wire form is small enough to write by hand:

    field 1, wire type 2  ->  0x0A, varint length, bytes

so the comparison is between two transports carrying identical bytes.
"""

import argparse
import json
import sys
import time

import grpc

sys.path.insert(0, "/spike")
import client as spike  # noqa: E402  - path is set above

FULL_METHOD = "/trendinvesting.spike.Transport/Decide"


def _varint(value):
    out = bytearray()
    while True:
        chunk = value & 0x7F
        value >>= 7
        out.append(chunk | (0x80 if value else 0))
        if not value:
            return bytes(out)


def encode_bytes_value(payload):
    return b"\x0a" + _varint(len(payload)) + payload


def decode_bytes_value(buf):
    if not buf:
        return b""
    if buf[0] != 0x0A:
        raise ValueError("unexpected field tag {}".format(buf[0]))
    shift = 0
    length = 0
    index = 1
    while True:
        byte = buf[index]
        length |= (byte & 0x7F) << shift
        index += 1
        if not byte & 0x80:
            break
        shift += 7
    return buf[index:index + length]


def percentile(sorted_samples, p):
    rank = (p * len(sorted_samples) + 99) // 100
    rank = max(1, min(rank, len(sorted_samples)))
    return sorted_samples[rank - 1]


def main(argv=None):
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--socket", default="/run/spike/grpc.sock")
    parser.add_argument("--rounds", type=int, default=500)
    parser.add_argument("--warmup", type=int, default=50)
    parser.add_argument("--universe", type=int, default=1000)
    parser.add_argument("--label", default="python-grpc")
    args = parser.parse_args(argv)

    options = [
        ("grpc.max_send_message_length", 8 << 20),
        ("grpc.max_receive_message_length", 8 << 20),
    ]
    channel = grpc.insecure_channel("unix://" + args.socket, options=options)
    call = channel.unary_unary(FULL_METHOD, request_serializer=None, response_deserializer=None)

    sequence = 1
    samples = []
    request_bytes = 0
    for round_index in range(args.warmup + args.rounds):
        bar = spike.envelope("market.bar.completed", sequence,
                             spike.bar_payload(args.universe, sequence))
        sequence += 1
        # Everything the adapter must do per bar is inside the timer, exactly
        # as in bench.py: encode, send, wait, decode, check causation. Timing
        # only the wire call would flatter gRPC against a client that encodes
        # inside its own measured exchange.
        started = time.perf_counter()
        encoded = json.dumps(bar, separators=(",", ":")).encode("utf-8")
        raw = call(encode_bytes_value(encoded))
        decision = json.loads(decode_bytes_value(raw))
        if decision["causation_id"] != bar["id"]:
            raise SystemExit("reply does not answer the request")
        elapsed = (time.perf_counter() - started) * 1000.0
        if round_index >= args.warmup:
            samples.append(elapsed)
            request_bytes = len(encoded) + 1

    samples.sort()
    print(
        "{} universe={} n={} req={}B min={:.3f}ms median={:.3f}ms p95={:.3f}ms "
        "p99={:.3f}ms max={:.3f}ms mean={:.3f}ms".format(
            args.label, args.universe, len(samples), request_bytes, samples[0],
            percentile(samples, 50), percentile(samples, 95), percentile(samples, 99),
            samples[-1], sum(samples) / len(samples),
        )
    )
    return 0


if __name__ == "__main__":
    sys.exit(main())
