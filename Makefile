.PHONY: test test-race lint fmt check crosscompile

# test runs the whole suite, including the end-to-end tests that compile real
# throwaway binaries and update one into another.
test:
	go test ./... -count=1

test-race:
	go test ./... -count=1 -race

lint:
	go vet ./...

fmt:
	gofmt -s -w .

# crosscompile is worth its few seconds: the Windows-only files carry most of
# the platform-specific machinery and are the easiest to break from a Unix host.
crosscompile:
	@for target in linux/amd64 linux/arm64 darwin/amd64 darwin/arm64 windows/amd64 windows/arm64; do \
		echo "  $$target"; \
		GOOS=$${target%/*} GOARCH=$${target#*/} go build ./... || exit 1; \
	done

check: lint test crosscompile
