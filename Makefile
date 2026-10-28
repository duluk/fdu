# fdu: `make` builds ./fdu for this machine; `make help` lists the rest.

BINARY := fdu
DIST   := dist
GO     ?= go

# Release builds: no symbol table or DWARF, no local paths baked in.
LDFLAGS := -s -w
GOFLAGS := -trimpath

# GOOS/GOARCH pairs for `make cross`, and the file name each one gets.
PLATFORMS := \
	darwin/arm64:fdu-macos-arm64 \
	darwin/amd64:fdu-macos-intel \
	linux/amd64:fdu-linux-amd64 \
	linux/arm64:fdu-linux-arm64 \
	windows/amd64:fdu.exe

.DEFAULT_GOAL := build
.PHONY: build install run test vet fmt fmt-check check cross clean help

build: ## Build ./fdu for this machine
	$(GO) build $(GOFLAGS) -ldflags '$(LDFLAGS)' -o $(BINARY) .

install: ## Install fdu into $(go env GOPATH)/bin
	$(GO) install $(GOFLAGS) -ldflags '$(LDFLAGS)' .

run: build ## Build and run, e.g. make run ARGS='-suggest ~'
	./$(BINARY) $(ARGS)

test: ## Run the tests
	$(GO) test ./...

vet: ## Run go vet for every supported platform
	@for p in $(PLATFORMS); do \
		target=$${p%%:*}; \
		echo "vet $$target"; \
		GOOS=$${target%/*} GOARCH=$${target#*/} $(GO) vet ./... || exit 1; \
	done

fmt: ## Format the source
	gofmt -w .

fmt-check: ## Fail if any file needs formatting
	@out=$$(gofmt -l .); if [ -n "$$out" ]; then echo "needs gofmt:"; echo "$$out"; exit 1; fi

check: fmt-check vet test ## Everything CI would run: format, vet, tests

cross: ## Build every platform into dist/
	@mkdir -p $(DIST)
	@for p in $(PLATFORMS); do \
		target=$${p%%:*}; out=$${p#*:}; \
		echo "build $$target -> $(DIST)/$$out"; \
		GOOS=$${target%/*} GOARCH=$${target#*/} CGO_ENABLED=0 \
			$(GO) build $(GOFLAGS) -ldflags '$(LDFLAGS)' -o $(DIST)/$$out . || exit 1; \
	done

clean: ## Remove build output
	rm -rf $(BINARY) $(BINARY).exe $(DIST)

help: ## List targets
	@grep -E '^[a-z-]+:.*## ' $(MAKEFILE_LIST) | awk -F':.*## ' '{printf "  %-10s %s\n", $$1, $$2}'
