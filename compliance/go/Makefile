# Build the compliance collector.
#
#   make build   compile to ../bin/compliance (where run.sh looks first)
#   make test    unit tests
#   make parity  differential test against the Python runner (needs python + PyYAML)
#   make check   fmt + vet + test
#   make clean   remove build artefacts
#
# The binary is placed in compliance/bin/, which is gitignored: it is a build
# output, not a source artefact, and a stale binary in the tree is exactly the
# kind of thing that makes a "port" quietly stop matching its source.

BIN     := ../bin/compliance
PKG     := ./cmd/compliance
PARITY  := ../tests/parity.py
PYTHON  ?= python3

.PHONY: build test parity check clean fmt vet tidy

build:
	@mkdir -p ../bin
	go build -trimpath -ldflags "-s -w" -o $(BIN) $(PKG)
	@echo "built $(BIN)"

test:
	go test ./...

parity: build
	$(PYTHON) $(PARITY) ../bin/compliance

fmt:
	gofmt -w .

vet:
	go vet ./...

check: fmt vet test

tidy:
	go mod tidy

clean:
	rm -f $(BIN)
	rm -rf ../bin
