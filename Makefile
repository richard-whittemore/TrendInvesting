export GOTOOLCHAIN := local

COVERAGE_MIN ?= 80.0
COVERAGE_PROFILE ?= coverage.out

.PHONY: build check coverage deps fmt fmt-check golangci lint staticcheck test vet vuln

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

check: deps lint coverage vuln build
