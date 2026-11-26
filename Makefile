.PHONY: all build web test test-go test-web e2e bench browser clean install

all: web build

web:
	cd web && pnpm install --frozen-lockfile && pnpm build

build:
	go build -o bin/atlas ./cmd/atlas
	go build -o bin/atlasd ./cmd/atlasd

test: test-go test-web

test-go:
	go vet ./...
	go test ./...

test-web:
	cd web && pnpm exec tsc -b --noEmit && pnpm test

# Real Claude Code end-to-end (uses `claude -p` with an isolated --settings file; spends a few tokens).
e2e: build
	ATLAS_E2E_CLAUDE=1 go test ./tests/integration -run TestLiveClaude -v -count=1

bench:
	go test ./tests/integration -run '^(TestBurstIngestionAndStreamLatency|TestHookOverhead)$$' -count=1 -v

browser: all
	node tests/browser/smoke.mjs bin

install: all
	install -m 0755 bin/atlas bin/atlasd /usr/local/bin/

clean:
	rm -rf bin
