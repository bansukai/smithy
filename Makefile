all:: bin/smithy

bin/smithy: *.go cmd/smithy/*.go
	mkdir -p bin
	go build -ldflags "-X github.com/bansukai/smithy.ToolVersion=`git describe --tag`" -o bin/smithy github.com/bansukai/smithy/cmd/smithy

install:: all
	rm -f $(HOME)/bin/smithy
	cp -p bin/smithy $(HOME)/bin/smithy

test::
#	go test github.com/bansukai/smithy/test

proper::
	go fmt github.com/bansukai/smithy
	go vet github.com/bansukai/smithy
	go fmt github.com/bansukai/smithy/cmd/smithy
	go vet github.com/bansukai/smithy/cmd/smithy

clean::
	rm -rf bin

go.mod:
	go mod init github.com/bansukai/smithy && go mod tidy
