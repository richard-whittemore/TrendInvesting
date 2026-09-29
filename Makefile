export GOTOOLCHAIN := local

COVERAGE_MIN ?= 80.0
COVERAGE_PROFILE ?= coverage.out

.PHONY: adapter-test build check coverage deps fmt fmt-check golangci lint research-test staticcheck test vet vuln

build:
	go build ./...

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

vuln:
	go tool govulncheck ./...

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
adapter-test: research-test
	cd adapter/lean && python3 -m unittest discover -s tests

# The research checks' rule cores (research/qc-cloud for the Turtle
# Baseline, research/qc-cloud-sublime for the Sublime control,
# research/qc-cloud-futures for Faith's own unadapted system on futures,
# and research/futures-local, the local Pinnacle CLC backtester that
# imports research/qc-cloud-futures/rules.py):
# standard-library Python only, no QuantConnect imports, so they run here
# without any LEAN environment. Each main.py (the QuantConnect algorithm)
# cannot be imported outside QuantConnect's own AlgorithmImports
# environment -- none of the three folders' local LEAN checks have futures
# data for the third one -- so research/qc-cloud and research/qc-cloud-futures
# are also syntax-checked here with python3 -m py_compile; every folder's
# own test_build_upload.py additionally compiles its stripped upload copy.
# The owner's first QuantConnect Cloud backtest is the real test of each
# folder's own QuantConnect API calls.
# research/edge-search is syntax-checked the same way; its timing script is
# also run against a stand-in AlgorithmImports (research/edge-search/README.md).
research-test:
	cd research/qc-cloud && python3 -m unittest discover -s . -p "test_*.py"
	python3 -m py_compile research/qc-cloud/main.py
	cd research/qc-cloud-sublime && python3 -m unittest discover -s . -p "test_*.py"
	cd research/qc-cloud-futures && python3 -m unittest discover -s . -p "test_*.py"
	python3 -m py_compile research/qc-cloud-futures/main.py
	cd research/edge-search/timing && python3 -m unittest discover -s . -p "test_*.py"
	cd research/edge-search && python3 -m unittest test_followup
	python3 -m py_compile research/edge-search/boost/template.py research/edge-search/boost/mix_template.py \
		research/edge-search/shorting/trend_template.py research/edge-search/shorting/factors_ls.py \
		research/edge-search/futures/carry.py research/edge-search/futures/blend.py
	python3 -m py_compile research/edge-search/etf-trend/main.py research/edge-search/etf-trend/main_oos.py \
		research/edge-search/factors/main.py research/edge-search/timing/main.py \
		research/edge-search/riskparity/main.py research/edge-search/bh-rsp/main.py
	cd research/futures-local && python3 -m unittest discover -s . -p "test_*.py"

check: deps lint coverage vuln build adapter-test research-test
