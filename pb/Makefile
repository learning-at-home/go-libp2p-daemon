pbgos := $(patsubst %.proto,%.pb.go,$(wildcard *.proto))

all: $(pbgos)

%.pb.go: %.proto
	protoc --gogofast_out=. --proto_path=$(GOPATH)/src:. $<

# Actually, I compiled it manually! Run "protoc -I=. --go_out=. ./p2pd.proto" in pb folder
# protoc --gofast_out=. p2pd.proto