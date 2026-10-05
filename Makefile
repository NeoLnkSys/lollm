# LoLLM — single-binary AI gateway / LLM router
GO      ?= go
BINARY  := bin/lollm
VERSION ?= 0.1.6
COMMIT  ?= $(shell git rev-parse --short HEAD 2>/dev/null || echo none)
DATE    := $(shell date -u +%Y-%m-%dT%H:%M:%SZ)
MODULE  := github.com/lollm/lollm
LDFLAGS := -s -w \
	-X $(MODULE)/internal/cli.Version=$(VERSION) \
	-X $(MODULE)/internal/cli.Commit=$(COMMIT) \
	-X $(MODULE)/internal/cli.BuildDate=$(DATE)

# modernc.org/libc is enormous; on small machines (2GB RAM) parallel
# compilation OOMs. Build serially and cap the Go GC heap. Override via:
#   make build P=4 MEM= 
P       ?= 1
MEM     ?= 1500MiB
GOENV   := GOGC=50 GOMEMLIMIT=$(MEM)

.PHONY: build run test vet tools docker release clean

## build: produce the static single binary ./bin/lollm
build:
	$(GOENV) CGO_ENABLED=0 $(GO) build -p $(P) -trimpath -ldflags "$(LDFLAGS)" -o $(BINARY) ./cmd/lollm

## tools: build the dev mock provider (./bin/mock-upstream)
tools:
	$(GOENV) CGO_ENABLED=0 $(GO) build -p $(P) -trimpath -ldflags "-s -w" -o bin/mock-upstream ./cmd/mock-upstream

## run: build then start the gateway
run: build
	./$(BINARY) serve

## test: run all unit + integration tests
test:
	$(GOENV) CGO_ENABLED=0 $(GO) test -p $(P) ./...

## vet: static analysis
vet:
	$(GO) vet ./...

## docker: multi-stage docker image (requires docker)
docker:
	docker build --build-arg VERSION=$(VERSION) -t lollm-synapse:$(VERSION) .

## release: cross-compile static tarballs into release/
release:
	@mkdir -p release
	@for t in linux-amd64 darwin-amd64 darwin-arm64 windows-amd64; do \
	  os=$${t%%-*}; arch=$${t##*-}; ext=""; \
	  [ "$$os" = "windows" ] && ext=".exe"; \
	  echo "==> lollm-$(VERSION)-$$t"; \
	  $(GOENV) CGO_ENABLED=0 GOOS=$$os GOARCH=$$arch $(GO) build -p $(P) -trimpath \
	    -ldflags "$(LDFLAGS)" -o release/lollm$$ext ./cmd/lollm || exit 1; \
	  mkdir -p release/stage && mv release/lollm$$ext release/stage/; \
	  cp README.md CHANGELOG.md release/stage/ 2>/dev/null || true; \
	  cp configs/config.example.yaml configs/backup.example.json release/stage/ 2>/dev/null || true; \
	  tar -czf release/lollm-$(VERSION)-$$t.tar.gz -C release/stage . || exit 1; \
	  rm -rf release/stage; \
	done
	@echo "release artifacts:"; ls -la release/

clean:
	rm -rf bin release
