export GOTOOLCHAIN := local

COVERAGE_MIN ?= 80.0
COVERAGE_PROFILE ?= coverage.out

.PHONY: adapter-test build check coverage deps fmt fmt-check golangci lint staticcheck test vet vuln

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
adapter-test:
	cd adapter/lean && python3 -m unittest discover -s tests

check: deps lint coverage vuln build adapter-test
