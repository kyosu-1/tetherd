BIN := bin
PKGS := ./...
LDFLAGS := -X github.com/kyosu-1/tetherd/internal/version.Version=$(shell git describe --tags --always --dirty)

.PHONY: build test lint clean e2e-local

build:
	mkdir -p $(BIN)
	go build -ldflags '$(LDFLAGS)' -o $(BIN)/tetherd ./cmd/tetherd
	go build -ldflags '$(LDFLAGS)' -o $(BIN)/tetherd-agent ./cmd/tetherd-agent
	go build -ldflags '$(LDFLAGS)' -o $(BIN)/tetherd-helper ./cmd/tetherd-helper
	go build -ldflags '$(LDFLAGS)' -o $(BIN)/tetherd-exec ./cmd/tetherd-exec

test:
	go test -race -count=1 $(PKGS)

lint:
	go vet $(PKGS)

clean:
	rm -rf $(BIN)

e2e-local: build
	bash hack/e2e-local.sh
