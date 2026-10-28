BINARY  := resmon
GO      ?= go
PREFIX  ?= $(HOME)/.local
BINDIR  ?= $(PREFIX)/bin
PKG     := ./cmd/resmon
SOURCES := $(filter-out %_test.go,$(wildcard cmd/resmon/*.go)) $(wildcard cmd/resmon/*.m) go.mod

# Extra flags for run targets, e.g. `make run ARGS="-d 30s -ai"`.
ARGS ?=

.PHONY: all build test vet fmt check run run-root watch install uninstall clean help

all: build

build: $(BINARY) ## Build ./resmon (default)

$(BINARY): $(SOURCES)
	$(GO) build -o $@ $(PKG)

test: ## Run unit tests
	$(GO) test ./...

vet: ## Run go vet
	$(GO) vet ./...

fmt: ## Format the Go sources in place
	gofmt -w cmd

check: vet test ## Fail if anything is unformatted, then vet and test
	@unformatted=$$(gofmt -l .); \
	if [ -n "$$unformatted" ]; then echo "gofmt needed:"; echo "$$unformatted"; exit 1; fi

run: $(BINARY) ## Build and run a 15s analysis (pass flags via ARGS=...)
	./$(BINARY) $(ARGS)

run-root: $(BINARY) ## Same as run, with sudo for full system-process detail
	sudo ./$(BINARY) $(ARGS)

watch: $(BINARY) ## Build and run continuous monitoring until Ctrl-C
	./$(BINARY) -watch $(ARGS)

install: $(BINARY) ## Install to ~/.local/bin (override with PREFIX=... or BINDIR=...)
	install -d $(BINDIR)
	install -m 0755 $(BINARY) $(BINDIR)/$(BINARY)

uninstall: ## Remove the installed binary
	rm -f $(BINDIR)/$(BINARY)

clean: ## Remove the built binary
	rm -f $(BINARY)

help: ## List targets
	@awk 'BEGIN {FS = ":.*## "} /^[a-z-]+:.*## / {printf "  %-10s %s\n", $$1, $$2}' $(MAKEFILE_LIST)
