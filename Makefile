BIN=tcp-flowgen
SRC_PROTO=$(shell find . -name '*.proto')
SRC_GO=$(wildcard *.go) $(SRC_PROTO:.proto=.pb.go)

.PHONY: fmt clean master client1 client2
default: $(BIN)

$(BIN): $(SRC_GO) $(SRC_PROTO)
	go build -o $(BIN)

messages/messages.pb.go: messages/messages.proto
	protoc -I messages/ $< --go_out=plugins=grpc:messages

python/messages_pb2.py: messages/messages.proto
	protoc -I messages/ --python_out=. $<
	mv $(notdir $@) $@

fmt:
	goimports -w $(wildcard *.go)

clean:
	rm -rf $(BIN) $(SRC_PROTO:.proto=.pb.go)

# ------------------------------------------------------------------------------
# Tests

master: $(BIN)
	./$(BIN) -master examples/flows_example.json -keepalive -output results.pb.gz

client1: $(BIN)
	./$(BIN) -listen :8841
	
client2: $(BIN)
	./$(BIN) -listen :8842
