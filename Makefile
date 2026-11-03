# Foundry monorepo Makefile. See CLAUDE.md for the command reference.
GO        ?= go
GOFLAGS   ?= -mod=vendor
export GOTOOLCHAIN=local
export GOFLAGS
BIN       := bin
SPEC      ?= test/golden/specs/vm-server.yaml
WORK      ?= /var/lib/foundry/builds

IMAGE     ?=
MATRIX    ?= uefi-tpm-reboot
KEYS      ?= out/keys
PHASE0    ?= out/phase0
export FOUNDRY_TOOLS_TREE ?= /var/lib/foundry/tools-tree
export FOUNDRY_DIRECT_MIRRORS ?= 1

.PHONY: dev-up dev-down e2e test-crash test-repro phase1 all build components test test-race lint golden golden-update web web-test image sign-image boot-test phase0 tools-tree host-deps clean

all: build

build:
	$(GO) build -o $(BIN)/ ./cmd/...

components:
	mkdir -p $(BIN)/components
	CGO_ENABLED=0 $(GO) build -trimpath -ldflags "-s -w" -o $(BIN)/components/foundry-agent ./cmd/foundry-agent
	CGO_ENABLED=0 $(GO) build -trimpath -ldflags "-s -w" -o $(BIN)/components/foundry-installer ./cmd/foundry-installer

test:
	$(GO) test ./...

test-race:
	$(GO) test -race ./...

lint:
	$(GO) vet ./...
	@test -z "$$(gofmt -l cmd internal)" || (gofmt -l cmd internal; echo "gofmt needed"; exit 1)
	shellcheck -S warning internal/compiler/files/initrd/initrd-repart internal/compiler/files/health/health internal/compiler/files/health/boottest-report scripts/*.sh

golden:
	$(GO) test ./internal/compiler/ -run TestGolden

golden-update:
	$(GO) test ./internal/compiler/ -run TestGolden -update

tools-tree:
	scripts/make-tools-tree.sh /var/lib/foundry/tools-tree

host-deps:
	scripts/host-deps.sh

image: build components
	$(BIN)/fctl build $(SPEC) -w $(WORK) $(ARGS)

# make sign-image IMAGE=dir/ci-server_1.0.0.raw  (UKI and roothash are found next to it)
sign-image: build
	$(BIN)/signer sign-image -keys $(KEYS) -tenant dev -disk $(IMAGE) -uki $(IMAGE:.raw=.efi) -roothash @$(IMAGE:.raw=.roothash) -out $(dir $(IMAGE))signed

# make boot-test IMAGE=disk.raw MATRIX=uefi-tpm-reboot,sb-tpm-reboot (sb-* needs a signed image)
boot-test: build
	$(BIN)/tester -image $(IMAGE) -scenario $(MATRIX) $(ARGS)

# Phase 0 acceptance suite: builds v1+v2, signs them, runs all boot tests.
phase0: build components
	scripts/phase0-acceptance.sh $(PHASE0)

web:
	cd web && npm run build

web-test:
	cd web && npm test

# Phase 1 acceptance (needs Postgres: scripts/pg-dev.sh; ~30 min without KVM).
# `make test` already covers acceptance 1 (every endpoint hit by an integration
# test, enforced in internal/api/main_test.go) and 6 (overlay path traversal → 422).
dev-up: build components
	scripts/dev-up.sh
dev-down:
	scripts/dev-down.sh
e2e: dev-up
	FOUNDRY_E2E_URL=http://127.0.0.1:8080 $(GO) test -tags e2e -timeout 3h -count=1 -v ./test/e2e/
test-crash:   # acceptance 5: SIGKILL the worker mid-build (set PROJECT=prj_… from `fctl projects`)
	scripts/test-worker-crash.sh test/golden/specs/vm-server.yaml $(PROJECT)
test-repro:   # acceptance 4: same spec twice → same /usr root hash
	scripts/test-reproducible.sh $(PROJECT)
phase1: test e2e

clean:
	rm -rf $(BIN) web/dist out
