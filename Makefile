BINARY   := micelio
CMD      := ./cmd/micelio
VERSION  := $(shell git describe --tags --always --dirty 2>/dev/null || echo "dev")
LDFLAGS  := -s -w -X main.version=$(VERSION)

.PHONY: all build clean test proto linux-amd64 linux-arm64 linux-386 darwin-arm64

all: build

proto:
	protoc --go_out=. --go_opt=module=micelio proto/micelio.proto

build:
	CGO_ENABLED=0 go build -ldflags "$(LDFLAGS)" -o bin/$(BINARY) $(CMD)

test:
	go test ./...

clean:
	rm -rf bin/

# Cross-compilation targets — static binaries, zero external deps
linux-amd64:
	CGO_ENABLED=0 GOOS=linux GOARCH=amd64 go build -ldflags "$(LDFLAGS)" -o bin/$(BINARY)-linux-amd64 $(CMD)

linux-arm64:
	CGO_ENABLED=0 GOOS=linux GOARCH=arm64 go build -ldflags "$(LDFLAGS)" -o bin/$(BINARY)-linux-arm64 $(CMD)

linux-386:
	CGO_ENABLED=0 GOOS=linux GOARCH=386 go build -ldflags "$(LDFLAGS)" -o bin/$(BINARY)-linux-386 $(CMD)

darwin-arm64:
	CGO_ENABLED=0 GOOS=darwin GOARCH=arm64 go build -ldflags "$(LDFLAGS)" -o bin/$(BINARY)-darwin-arm64 $(CMD)

release: linux-amd64 linux-arm64 linux-386 darwin-arm64
