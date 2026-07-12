SHELL := /bin/sh

.PHONY: generate fmt build release-check test test-go test-integration test-desktop test-android lint run-core run-desktop android-debug connector-simulators clean

generate:
	./scripts/generate-protocol.sh

fmt:
	gofmt -w $$(find . -name '*.go' -not -path './apps/*')
	cd apps/desktop && npm run format --if-present

build: generate
	mkdir -p build/bin
	go build -trimpath -o build/bin/veqri-core ./cmd/veqri-core
	go build -trimpath -o build/bin/veqri ./cmd/veqri-cli
	cd apps/desktop && npm ci && npm run build

release-check: build
	cd apps/desktop && npm run native:build
	go run ./scripts/release-smoke.go

test: test-go test-desktop test-android

test-go:
	go test -race ./...

test-integration:
	go test -race ./tests/integration/... ./tests/e2e/...

test-desktop:
	cd apps/desktop && npm ci && npm run typecheck && npm test -- --run && npm run build

test-android:
	cd apps/android && ./gradlew --no-daemon testDebugUnitTest lintDebug assembleDebug assembleRelease assembleDebugAndroidTest

lint:
	go vet ./...
	cd apps/desktop && npm run typecheck
	cd apps/android && ./gradlew --no-daemon lintDebug

run-core:
	go run ./cmd/veqri-core

run-desktop:
	cd apps/desktop && npm run dev

android-debug:
	cd apps/android && ./gradlew --no-daemon assembleDebug

connector-simulators:
	./scripts/simulate-connectors.sh

clean:
	go clean -testcache
	rm -rf build apps/desktop/dist apps/desktop/node_modules apps/android/app/build
