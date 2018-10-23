#!/usr/bin/env python

import gzip
import struct
import argparse
import messages_pb2 as pb


def parse_pb(path):
    print("Parsing " + path)
    if path.endswith('.gz'):
        f = gzip.open(path, 'rb')
    else:
        f = open(path, 'rb')

    flows = []
    while True:
        # Read message size
        buf = f.read(4)
        if len(buf) == 0:
            break

        # Convert message size to integer
        sz = struct.unpack('!I', buf)[0]

        # Read serialized protobuf
        flow = pb.Flow()
        flow.ParseFromString(f.read(sz))
        flows.append(flow)

    f.close()
    return flows


if __name__ == "__main__":
    p = argparse.ArgumentParser()
    p.add_argument('input', type=str)
    args = p.parse_args()
    parse_pb(args.input)
