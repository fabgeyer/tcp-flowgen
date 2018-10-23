# Distributed TCP traffic generator

## Architecture

The traffic generator is based on a master-slaves architecture.
The role of the master is to setup TCP flows on the different slaves, start the flows between the slaves, and finally collect the results at the end of the experiment.

In order to split management packets from measurement packets, each node is configured using two IP address:

- The `mgmt` IP address is used for management,
- The `host` IP address is used for making the tests and generating the TCP traffic.

Each flow is defined using the following parameters:

- `id`: Flow identified
- `src`: Slave used for source
- `dst`: Slave used for destination
- `duration`: Flow duration
- `start`: Flow start point (absolute time)
- `ccalg`: TCP Congestion control used for the flow
- `ecn`: Use the TCP ECN option or not
- `nodelay`: Use the TCP NoDelay option or not

Flow duration and start support units as describe in the [ParseDuration](https://golang.org/pkg/time/#ParseDuration) function (eg.: "10s", "10m30s").

At the end of the experiment, a result file is saved containing the following TCP values, saved at periodic interval:

- `castate`: state of congestion avoidance
- `state`: TCP state machine status (eg. Closed, Listen, SYN Sent, SYN Received, Established)
- `RTT`: round-trip time
- `RTTVar`: round-trip time variation
- `ThruBytesAcked`: # of bytes for which cumulative acknowledgments have been received
- `ThruBytesReceived`: # of bytes for which cumulative acknowledgments have been sent
- `UnackedSegs`: # of unack'd segments
- `LostSegs`: # of lost segments
- `RetransSegs`: # of retransmitting segments in transmission queue
- `ReorderedSegs`: # of reordered segments allowed

Additional statistics may also be added (see [`tcpinfo`'s documentation](https://godoc.org/github.com/mikioh/tcpinfo)).


## Parsing result files in python

The `python` folder contains an example of how to parse the result file in Python.
To run it, the `messages_pb2.py` file first has to be generated with:
```
$ make python/messages_pb2.py
```


## Compilation

Install required packages:
```
$ apt install golang protobuf-compiler
```
Set `GOPATH` variable (see also [the official Go documentation](https://golang.org/doc/code.html#GOPATH)):
```
$ export GOPATH=$HOME/go
```
Download dependencies:
```
$ go get
```
Download protobuf:
```
$ go get -u github.com/golang/protobuf/protoc-gen-go
```
Update your `PATH` environment variable for using the protobuf Go compiler:
```
$ export PATH=$PATH:$GOPATH/bin
```
Finally build binary:
```
$ make
```

## Example

In the example configuration file in `examples/flows_example.json`, two slaves are started and the master creates two flows between the slaves.

```
$ ./tcp-flowgen -listen localhost:8841
$ ./tcp-flowgen -listen localhost:8842
$ ./tcp-flowgen -master examples/flows_example.json
```
