BINARY := rgitlog
GOBIN  ?= $(shell go env GOPATH)/bin

.PHONY: all build install vet fmt check clean

all: build

build:
	go build -trimpath -o $(BINARY) .

install:
	go build -trimpath -o $(GOBIN)/$(BINARY) .

vet:
	go vet ./...

fmt:
	gofmt -w .

# Fails if any file needs formatting or vet reports a problem.
check:
	@test -z "$$(gofmt -l .)" || { echo "needs gofmt:"; gofmt -l .; exit 1; }
	go vet ./...

clean:
	rm -f $(BINARY)
