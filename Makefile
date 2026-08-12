NUON_REPO_ROOT ?= /Users/harsh/work/nuonco/mono
EXTENSION_PKG := ./bins/nuon-bundle
EXTENSION_PKGS := ./bins/nuon-bundle/...
BINARY := nuon-ext-bundle

.DEFAULT_GOAL := build

.PHONY: build test fmt vet clean check-repo

check-repo:
	@test -f "$(NUON_REPO_ROOT)/go.mod" || \
		(echo "NUON_REPO_ROOT must point to the Nuon monorepo root (missing go.mod at $(NUON_REPO_ROOT))" && exit 1)

build: check-repo
	go -C "$(NUON_REPO_ROOT)" build -o "$(CURDIR)/$(BINARY)" "$(EXTENSION_PKG)"

test: check-repo
	go -C "$(NUON_REPO_ROOT)" test "$(EXTENSION_PKGS)"

fmt: check-repo
	go -C "$(NUON_REPO_ROOT)" fmt "$(EXTENSION_PKGS)"

vet: check-repo
	go -C "$(NUON_REPO_ROOT)" vet "$(EXTENSION_PKGS)"

clean:
	rm -f "$(BINARY)"
