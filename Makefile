VERSION ?= $(shell git describe --tags --always --dirty 2>/dev/null || echo dev)
LDFLAGS := -s -w -X main.version=$(VERSION)
TARGETS := linux/amd64 linux/arm64 darwin/amd64 darwin/arm64 windows/amd64

.PHONY: build test check release clean

build:
	go build -trimpath -ldflags "$(LDFLAGS)" -o cloudundo ./cmd/cloudundo

test:
	go test -race ./...

check: test
	go vet ./...
	test -z "$$(gofmt -l .)"
	go test -run='^$$' -fuzz=FuzzAnalyze -fuzztime=30s ./internal/effects
	go test -run='^$$' -fuzz=FuzzHookInput -fuzztime=30s ./cmd/cloudundo
	go build -o dist/test-cloudundo ./cmd/cloudundo && test/plugin.sh dist/test-cloudundo

# The files a GitHub release needs: one binary per system and checksums.txt,
# which install.sh checks every download against.
release: clean
	mkdir -p dist
	@for t in $(TARGETS); do \
		os=$${t%/*}; arch=$${t#*/}; ext=; [ $$os = windows ] && ext=.exe; \
		echo "building $$os/$$arch"; \
		CGO_ENABLED=0 GOOS=$$os GOARCH=$$arch go build -trimpath -ldflags "$(LDFLAGS)" \
			-o dist/cloudundo-$$os-$$arch$$ext ./cmd/cloudundo || exit 1; \
	done
	cd dist && sha256sum cloudundo-* > checksums.txt
	@cat dist/checksums.txt

clean:
	rm -rf dist cloudundo
