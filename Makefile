export GOTOOLCHAIN := local

COVERAGE_MIN ?= 80.0
COVERAGE_PROFILE ?= coverage.out

.PHONY: adapter-test build check coverage deploy-test demo-boost-dry-run deps engine fmt fmt-check golangci lint research-test staticcheck test vet vuln

# The build stamp: every envelope's strategy version carries it (ADR 0016),
# and cmd/engine refuses -mode dry-run, paper or live on an unstamped build
# ("dev"). scripts/build-stamp.sh prints `git describe --always --dirty`,
# with a dirty tree's own tree hash appended, and "dev" outside a git
# checkout, which those modes refuse. Expanded only by the targets that
# link a binary.
BUILD_STAMP ?= $(shell ./scripts/build-stamp.sh)
LDFLAGS = -X github.com/richard-whittemore/TrendInvesting/internal/buildinfo.Version=$(BUILD_STAMP)

build:
	go build -ldflags "$(LDFLAGS)" ./...

# The engine binary, stamped. deploy/build-engine.sh builds the server's copy.
engine:
	go build -ldflags "$(LDFLAGS)" -o bin/ti-engine ./cmd/engine

# The one engine in dry-run over a synthetic SPY series, its decision log,
# and its durable log replayed byte for byte (scripts/demo-boost-dry-run.sh).
demo-boost-dry-run:
	./scripts/demo-boost-dry-run.sh

fmt:
	gofmt -w $$(find . -name '*.go' -not -path './vendor/*')

fmt-check:
	test -z "$$(gofmt -l $$(find . -name '*.go' -not -path './vendor/*'))"

test:
	go test -race -covermode=atomic -coverprofile=$(COVERAGE_PROFILE) ./...

coverage: test
	go tool cover -func=$(COVERAGE_PROFILE) | tee coverage.txt
	COVERAGE_MIN=$(COVERAGE_MIN) ./scripts/check-coverage.sh $(COVERAGE_PROFILE)

vet:
	go vet ./...

staticcheck:
	go tool staticcheck ./...

# Enforces the architectural and determinism rules from AGENTS.md
# (depguard, forbidigo) alongside the usual correctness linters.
golangci:
	go tool golangci-lint run ./...

lint: fmt-check vet staticcheck golangci

# GOVULNDB, when set, is a local copy of Go's vulnerability database
# (file:///...), for a check with no network: the triage job's gate
# (docs/manual/alerts-and-dashboard.md, "Alerts analysed by Claude").
vuln:
	go tool govulncheck $(if $(GOVULNDB),-db $(GOVULNDB)) ./...

deps:
	go mod tidy -diff
	go mod verify

# The LEAN adapter's suite, standard-library Python only. It includes the
# Go-to-Python decision contract and an end-to-end run against cmd/engine,
# so a Go change to an event the adapter reads fails here, not in LEAN.
# The arm64 CI job, which does not run make check, runs this target itself,
# so it depends on research-test too, rather than skip the research rule
# cores on that job. `check`'s own explicit research-test prerequisite,
# below, does not run it twice: make only remakes a phony target once per
# invocation.
# It also runs the IBKR adapter's suite (adapter/ibkr, ADR 0025), against a
# fake TWS API client: ibapi itself is not installed here.
adapter-test: research-test
	cd adapter/lean && python3 -m unittest discover -s tests
	cd adapter/ibkr && python3 -m unittest discover -s tests
	cd adapter && python3 -m unittest discover -s marketdata/tests -t .

# The research checks that stay in the build: research/edge-search, the
# search that produced Boost and Momentum. Its scripts that import
# AlgorithmImports cannot be imported outside QuantConnect's own
# environment, so they are syntax-checked here with python3 -m py_compile;
# its timing script is also run against a stand-in AlgorithmImports
# (research/edge-search/README.md).
# The Turtle, Sublime and futures research (research/history/) is kept as
# history and is not built or tested here; to run it, check out the tag
# turtle-final (research/history/README.md).
research-test:
	cd research/edge-search/timing && python3 -m unittest discover -s . -p "test_*.py"
	cd research/edge-search && python3 -m unittest test_followup test_parity_remaining
	cd research/edge-search/tax && python3 -m unittest test_tax_drag test_custodial
	cd research/edge-search/universe && python3 -m unittest test_universe
	cd research/edge-search/portfolio && python3 -m unittest test_blend
	cd research/edge-search/sectors && python3 -m unittest test_sectors
	python3 -m py_compile research/edge-search/boost/template.py research/edge-search/boost/mix_template.py \
		research/edge-search/shorting/trend_template.py research/edge-search/shorting/factors_ls.py \
		research/edge-search/futures/carry.py research/edge-search/futures/blend.py \
		research/edge-search/momentum/momentum.py research/edge-search/boost/lev_etf_template.py \
		research/edge-search/momentum/defensive.py research/edge-search/momentum/scores.py \
		research/edge-search/etf/rotation.py research/edge-search/momentum/defensive_v2.py \
		research/edge-search/momentum/universe_parity.py research/edge-search/momentum/parity_compare.py \
		research/edge-search/momentum/glide_path.py research/edge-search/momentum/golden_log.py \
		research/edge-search/boost/golden_log.py research/edge-search/momentum/fresh_universe.py \
		research/edge-search/momentum/parity_log.py research/edge-search/boost/parity_log.py \
		research/edge-search/momentum/parity_export.py research/edge-search/momentum/glide_parity_log.py \
		research/edge-search/momentum/minimum_lot_parity_log.py research/edge-search/momentum/ira_parity_log.py
	python3 -m py_compile research/edge-search/etf-trend/main.py research/edge-search/etf-trend/main_oos.py \
		research/edge-search/factors/main.py research/edge-search/timing/main.py \
		research/edge-search/riskparity/main.py research/edge-search/bh-rsp/main.py

# Shell tests for the deploy rehearsal, the provisioner's architecture
# rules and the server's build (no Docker needed; test-build.sh
# cross-compiles for linux/amd64 and linux/arm64).
deploy-test:
	./deploy/rehearsal/test-platform.sh
	./deploy/rehearsal/test-build.sh
	./deploy/rehearsal/test-release.sh
	./deploy/rehearsal/test-maintenance.sh
	./deploy/rehearsal/test-provision-arch.sh
	./deploy/rehearsal/test-provision-programs.sh
	./deploy/rehearsal/test-engine-configs.sh
	./deploy/mac-mini/test-programs-lib.sh
	cd deploy/rehearsal && python3 -m unittest test_twsapi_archive test_github_standin test_feed_month test_roth test_memory test_ira_replay test_ira_drills test_paper_diagnostic test_gateway_login
	cd deploy/tailscale && python3 -m unittest test_policy
	./deploy/rehearsal/test-clock.sh
	./deploy/rehearsal/test-target.sh
	./deploy/mac-mini/test-refusals.sh
	./deploy/rehearsal/test-operator-user.sh
	./deploy/rehearsal/test-ibc-modes.sh
	./deploy/rehearsal/test-gateway-guard.sh
	./deploy/rehearsal/test-adapter-venv.sh

# make check runs the momentum memory-budget test on its two-year fixture;
# a plain adapter-test runs the fourteen-month one (adapter/marketdata/tests/
# test_memory.py), which checks the same bound in about half the time.
check: export TI_MOMENTUM_MEMORY_FULL = 1
check: deps lint coverage vuln build adapter-test research-test deploy-test
