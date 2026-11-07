.PHONY: test smoke build

build:
	CGO_ENABLED=0 go build -trimpath -ldflags "-s -w" -o aperta ./cmd/aperta

test:
	go vet ./...
	go test -race ./...

smoke:
	go run ./tools/smoke -timeout 5m
