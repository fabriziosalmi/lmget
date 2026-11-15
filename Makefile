# lmget build and checks. `make ci` is what the CI box runs; it must pass
# before any tag.

GO       ?= go
VERSION  ?= $(shell git describe --tags --always --dirty 2>/dev/null || echo dev)
LDFLAGS  := -s -w -X github.com/fabriziosalmi/lmget.Version=$(VERSION)
SMOKE    ?= $(CURDIR)/.smoke

.PHONY: all build vet test race lint smoke ci release-snapshot clean

all: build

build:
	$(GO) build -trimpath -ldflags '$(LDFLAGS)' -o bin/lmget ./cmd/lmget

vet:
	$(GO) vet ./...
	@test -z "$$(gofmt -l $$(git ls-files '*.go'))" || { echo "gofmt needed:"; gofmt -l $$(git ls-files '*.go'); exit 1; }

test:
	$(GO) test -count=1 ./...

race:
	$(GO) test -race -count=1 ./...

# Secrets in the tree and in history, then repository hygiene gates.
lint:
	gitleaks detect --no-banner --redact
	lgit check . >/dev/null

# Live check against Hugging Face with a throwaway store and cache: plan,
# download, re-run (must fetch 0 bytes), offline sync through a dead proxy.
smoke: build
	rm -rf $(SMOKE) && mkdir -p $(SMOKE)/proj
	cd $(SMOKE)/proj && LMGET_HOME=$(SMOKE)/home HF_HUB_CACHE=$(SMOKE)/hf \
	  $(CURDIR)/bin/lmget sentence-transformers/all-MiniLM-L6-v2 --include '*.json' --include model.safetensors --lock --plain
	cd $(SMOKE)/proj && LMGET_HOME=$(SMOKE)/home HF_HUB_CACHE=$(SMOKE)/hf \
	  $(CURDIR)/bin/lmget sentence-transformers/all-MiniLM-L6-v2 --include '*.json' --include model.safetensors --plain 2>&1 \
	  | grep -q 'fetched=0 ' || { echo "second run downloaded again"; exit 1; }
	cd $(SMOKE)/proj && LMGET_HOME=$(SMOKE)/home HF_HUB_CACHE=$(SMOKE)/hf HTTPS_PROXY=http://127.0.0.1:9 HTTP_PROXY=http://127.0.0.1:9 \
	  $(CURDIR)/bin/lmget sync --plain
	LMGET_HOME=$(SMOKE)/home $(CURDIR)/bin/lmget doctor --verify --plain | grep -q '^ok	verify'
	rm -rf $(SMOKE)

ci: vet race build lint smoke

# The release archives as GitHub Actions builds them (reproducible), without
# publishing anything: compare dist/checksums.txt with the release's.
release-snapshot:
	./scripts/release-build.sh $(or $(RELEASE),v0.0.0) dist

clean:
	rm -rf bin dist $(SMOKE)
