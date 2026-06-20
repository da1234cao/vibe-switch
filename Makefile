# vibe-switch — behavioral test suite for an L2 switch under test.
# Network tests need root (netns / veth / AF_PACKET); run them under sudo.
#
# Common overrides:
#   make test SWITCH=bridge            # select switch under test (default: goswitch)
#   make test ARGS="-run TestVLAN -v"  # pass extra flags through to `go test`
#   make perf-gate PERF_MIN_PPS=50000  # perf run with a regression gate

# --- knobs ----------------------------------------------------------------
# SWITCH selects which "switch under test" the behavioral suite drives:
#   goswitch  our Go user-space switch (internal/goswitch)   [default]
#   bridge    Linux kernel bridge
# The test bodies never change — only this implementation behind them does.
SWITCH ?= goswitch
ARGS   ?=

# Env exported to every `go test` invocation.
TESTENV := SWITCH=$(SWITCH)

.DEFAULT_GOAL := help

# --- build / static checks (no root needed) -------------------------------
.PHONY: build
build: ## Compile all packages (no binary output)
	go build ./...

.PHONY: build-bin
build-bin: ## Build the standalone vibe-switch binary into ./bin
	go build -o ./bin/vibe-switch ./cmd/vibe-switch

.PHONY: demo
demo: build-bin ## Run the binary on a throwaway veth topology (needs root)
	./scripts/demo.sh

.PHONY: vet
vet: ## Run go vet
	go vet ./...

.PHONY: fmt
fmt: ## Format all Go sources in place
	go fmt ./...

.PHONY: fmt-check
fmt-check: ## Fail if any Go source is not gofmt-clean
	@unformatted=$$(gofmt -l .); \
	if [ -n "$$unformatted" ]; then \
		echo "not gofmt-clean:"; echo "$$unformatted"; exit 1; \
	fi

.PHONY: tidy
tidy: ## Sync go.mod/go.sum
	go mod tidy

# --- tests (need root) ----------------------------------------------------
.PHONY: test
test: ## Run the full behavioral suite (verbose)
	@echo "== switch under test: SWITCH=$(SWITCH) =="
	$(TESTENV) go test ./test -v $(ARGS)

.PHONY: test-l2
test-l2: ## Run only the L2 cases (learning, flooding, aging, ...)
	$(TESTENV) go test ./test -v -run 'TestL2|TestMAC|TestFlood|TestAge' $(ARGS)

.PHONY: test-vlan
test-vlan: ## Run only the VLAN cases (access/trunk/PVID)
	$(TESTENV) go test ./test -v -run TestVLAN $(ARGS)

.PHONY: test-perf
test-perf: ## Run the perf suite (reports only, no gate)
	$(TESTENV) go test ./test -v -run TestPerf $(ARGS)

.PHONY: perf-gate
perf-gate: ## Run perf with regression gates (set PERF_MIN_PPS / PERF_MAX_LOSS / PERF_MAX_P99_US)
	$(TESTENV) \
		$(if $(PERF_MIN_PPS),PERF_MIN_PPS=$(PERF_MIN_PPS)) \
		$(if $(PERF_MAX_LOSS),PERF_MAX_LOSS=$(PERF_MAX_LOSS)) \
		$(if $(PERF_MAX_P99_US),PERF_MAX_P99_US=$(PERF_MAX_P99_US)) \
		go test ./test -v -run TestPerf $(ARGS)

# --- housekeeping ---------------------------------------------------------
.PHONY: check
check: fmt-check vet build ## Static gate: gofmt + vet + build (no root)

.PHONY: clean
clean: ## Clean the Go build/test cache
	go clean -cache -testcache ./...

.PHONY: clean-netns
clean-netns: ## Remove leftover vs* netns/veth from interrupted test runs (needs root)
	@found=$$(ip netns list 2>/dev/null | awk '/^vs/{print $$1}'); \
	if [ -z "$$found" ]; then \
		echo "no leftover vs* netns"; \
	else \
		for ns in $$found; do ip netns del "$$ns" && echo "deleted netns $$ns"; done; \
	fi

.PHONY: help
help: ## Show this help
	@grep -hE '^[a-zA-Z0-9_-]+:.*?## .*$$' $(MAKEFILE_LIST) \
		| awk 'BEGIN {FS = ":.*?## "}; {printf "  \033[36m%-12s\033[0m %s\n", $$1, $$2}'
