pbgos := $(patsubst %.proto,%.pb.go,$(wildcard *.proto))

all: $(pbgos)

%.pb.go: %.proto
	protoc --gogofast_out=. --proto_path=$(GOPATH)/src:. $<

<<<<<<< HEAD
# Actually, I compiled it manually! Run "protoc -I=. --go_out=. ./p2pd.proto" in pb folder
# protoc --gofast_out=. p2pd.proto
=======
# Update:
# protoc -I=. --go_out=. ./p2pd.proto --go_opt=paths=source_relative
>>>>>>> 0b29642bd58095978b8261de0896021bec038ccd
