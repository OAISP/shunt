BIN     := shunt
HELPERS := internal/engine/bin
# Describe the working tree so a locally built binary identifies itself exactly.
VERSION := $(shell git describe --tags --always --dirty 2>/dev/null | sed 's/^v//')
LDFLAGS := -s -w
CLI_LDFLAGS := $(LDFLAGS) -X main.version=$(VERSION)

.PHONY: all build helpers test fmt vet clean install

all: helpers build

# The helper is embedded into the CLI, so it must be built first.
helpers:
	@mkdir -p $(HELPERS)
	GOOS=linux GOARCH=amd64 CGO_ENABLED=0 go build -trimpath -ldflags="$(LDFLAGS)" \
		-o $(HELPERS)/shunt-helper-linux-amd64 ./cmd/shunt-helper
	GOOS=linux GOARCH=arm64 CGO_ENABLED=0 go build -trimpath -ldflags="$(LDFLAGS)" \
		-o $(HELPERS)/shunt-helper-linux-arm64 ./cmd/shunt-helper
	@ls -lh $(HELPERS)/shunt-helper-* | awk '{print "  " $$9, $$5}'

build: helpers
	go build -trimpath -ldflags="$(CLI_LDFLAGS)" -o $(BIN) ./cmd/shunt
	@ls -lh $(BIN) | awk '{print "  " $$9, $$5}'

install: build
	install -m 0755 $(BIN) $(HOME)/.local/bin/$(BIN)
	@echo "  installed to $(HOME)/.local/bin/$(BIN)"

test:
	go test ./...

fmt:
	gofmt -w ./cmd ./internal

vet:
	go vet ./...

clean:
	rm -f $(BIN) $(HELPERS)/shunt-helper-*
