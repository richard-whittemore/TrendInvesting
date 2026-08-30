.PHONY: build check fmt test vet

build:
	go build ./...

fmt:
	gofmt -w $$(find . -name '*.go' -not -path './vendor/*')

test:
	go test -race ./...

vet:
	go vet ./...

check: vet test build
