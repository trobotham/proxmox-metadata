GO        ?= go
BUILD_DIR ?= build
LDFLAGS   ?= -s -w
GOFLAGS   ?= -trimpath

DESTDIR   ?=
SBINDIR   ?= /usr/local/sbin
CONFDIR   ?= /etc/proxmox-metadata
UNITDIR   ?= /etc/systemd/system
SVC_USER  ?= proxmox-metadata

BINARIES := $(BUILD_DIR)/proxmox-metadata $(BUILD_DIR)/proxmox-metadata-sync
GO_SRC   := $(shell find . -name '*.go' -not -path './$(BUILD_DIR)/*') go.mod
UNITS    := proxmox-metadata.service proxmox-metadata-sync.service proxmox-metadata-sync.timer

.PHONY: all build linux test vet fmt fmt-check shellcheck check clean \
        user install install-guest help

all: check build ## Run checks and build binaries

help: ## Show this help
	@grep -E '^[a-zA-Z_-]+:.*## ' $(MAKEFILE_LIST) | \
		awk 'BEGIN {FS = ":.*## "}; {printf "  %-14s %s\n", $$1, $$2}'

build: $(BINARIES) ## Build binaries for the current platform

$(BUILD_DIR)/proxmox-metadata: $(GO_SRC)
	@mkdir -p $(BUILD_DIR)
	$(GO) build $(GOFLAGS) -ldflags='$(LDFLAGS)' -o $@ .

$(BUILD_DIR)/proxmox-metadata-sync: $(GO_SRC)
	@mkdir -p $(BUILD_DIR)
	$(GO) build $(GOFLAGS) -ldflags='$(LDFLAGS)' -o $@ ./cmd/proxmox-metadata-sync

linux: ## Cross-compile linux/amd64 binaries into build/linux-amd64 (e.g. from macOS)
	$(MAKE) build BUILD_DIR=$(BUILD_DIR)/linux-amd64 GOOS=linux GOARCH=amd64 CGO_ENABLED=0

test: ## Run unit tests with the race detector
	$(GO) test -race -count=1 ./...

vet: ## Run go vet
	$(GO) vet ./...

fmt: ## Format Go sources
	gofmt -w $(filter %.go,$(GO_SRC))

fmt-check: ## Fail if Go sources are not gofmt-formatted
	@out="$$(gofmt -l $(filter %.go,$(GO_SRC)))"; \
	if [ -n "$$out" ]; then echo "gofmt needed:"; echo "$$out"; exit 1; fi

shellcheck: ## Lint the guest network helper
	bash -n guest/proxmox-network-helper
	shellcheck guest/proxmox-network-helper

check: test vet shellcheck ## Run tests, vet, and shellcheck

clean: ## Remove build output
	rm -rf $(BUILD_DIR)

user: ## Create the service account (run as root on the Proxmox host)
	getent group $(SVC_USER) >/dev/null || groupadd --system $(SVC_USER)
	getent passwd $(SVC_USER) >/dev/null || \
		useradd --system --gid $(SVC_USER) --home-dir /nonexistent \
			--shell /usr/sbin/nologin $(SVC_USER)

# Existing config.json and discovery.json are never overwritten: the sync job
# owns config.json and discovery.json holds local settings.
install: build ## Install binaries, configs, and units (run as root on the Proxmox host)
	install -d -o root -g root -m 0755 $(DESTDIR)$(SBINDIR) $(DESTDIR)$(UNITDIR)
	install -d -o root -g $(SVC_USER) -m 0750 $(DESTDIR)$(CONFDIR)
	install -o root -g root -m 0755 $(BINARIES) $(DESTDIR)$(SBINDIR)/
	install -o root -g root -m 0644 $(UNITS) $(DESTDIR)$(UNITDIR)/
	install -o root -g root -m 0644 README.md $(DESTDIR)$(CONFDIR)/README.md
	@for f in discovery config; do \
		dst=$(DESTDIR)$(CONFDIR)/$$f.json; \
		if [ -e "$$dst" ]; then echo "keeping existing $$dst"; \
		else install -o root -g $(SVC_USER) -m 0640 $$f.example.json "$$dst"; fi; \
	done
	@if [ -z "$(DESTDIR)" ] && command -v systemctl >/dev/null; then systemctl daemon-reload; fi

install-guest: ## Install the guest network helper (run as root inside a VM)
	install -m 0755 guest/proxmox-network-helper $(DESTDIR)$(SBINDIR)/proxmox-network-helper
	install -m 0644 guest/proxmox-network-helper.service guest/proxmox-network-helper.timer $(DESTDIR)$(UNITDIR)/
	@dst=$(DESTDIR)/etc/default/proxmox-network-helper; \
	if [ -e "$$dst" ]; then echo "keeping existing $$dst"; \
	else install -m 0644 guest/proxmox-network-helper.default "$$dst"; fi
	@if [ -z "$(DESTDIR)" ] && command -v systemctl >/dev/null; then systemctl daemon-reload; fi
