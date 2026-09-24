.PHONY: build test check

build:
	go build -trimpath -o bin/unreal-agent-runner ./cmd/unreal-agent-runner
	go build -trimpath -o bin/unreal_chat ./cmd/unreal_chat
	go build -trimpath -o bin/unreal-storage ./cmd/unreal-storage

test:
	go test -race ./...

check:
	go vet ./...
	@test -z "$$(gofmt -l cmd harness internal)" || { gofmt -l cmd harness internal; exit 1; }
