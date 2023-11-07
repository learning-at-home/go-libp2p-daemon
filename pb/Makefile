pbgos := $(patsubst %.proto,%.pb.go,$(wildcard *.proto))

all: $(pbgos)

%.pb.go: %.proto
	protoc --gogofast_out=. --proto_path=$(GOPATH)/src:. $<

# Update:
# protoc -I=. --go_out=. ./p2pd.proto --go_opt=paths=source_relative