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
# The arm64 CI job, which does not run make check, runs this target itself.
adapter-test:
	cd adapter/lean && python3 -m unittest discover -s tests

# research/qc-cloud's and research/qc-cloud-futures's rule cores (rules.py):
# standard-library Python only, no QuantConnect imports, so they run here
# without any LEAN environment. Each main.py (the QuantConnect algorithm
# that drives its own rules.py) cannot be imported outside QuantConnect's
# own AlgorithmImports environment -- neither folder's local LEAN checks
# have futures data for the second one -- so each is only syntax-checked
# here with python3 -m py_compile; the owner's first QuantConnect Cloud
# backtest is the real test of its QuantConnect API calls.
research-test:
	cd research/qc-cloud && python3 -m unittest discover -s . -p "test_*.py"
	python3 -m py_compile research/qc-cloud/main.py
	cd research/qc-cloud-futures && python3 -m unittest discover -s . -p "test_*.py"
	python3 -m py_compile research/qc-cloud-futures/main.py

check: deps lint coverage vuln build adapter-test research-test
