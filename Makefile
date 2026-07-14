SHELL := /bin/sh

.PHONY: generate generate-go generate-android check-generated check-generated-go check-generated-android fmt binaries build package package-all release-check test test-go test-integration test-desktop test-android lint run-core run-desktop android-debug connector-simulators clean

generate: generate-go generate-android

generate-go:
	./scripts/generate-protocol.sh

generate-android:
	cd apps/android && ./gradlew --no-daemon :protocol:regenerateAndroidProtocolBindings

check-generated: check-generated-go check-generated-android

check-generated-go:
	./scripts/check-generated-go.sh

check-generated-android:
	cd apps/android && ./gradlew --no-daemon :protocol:checkAndroidProtocolBindings

fmt:
	gofmt -w $$(find . -name '*.go' -not -path './apps/*')
	cd apps/desktop && npm run format --if-present

binaries: generate-go
	go run ./cmd/veqri-build binaries

build: binaries
	cd apps/desktop && npm ci && npm run build

package:
	go run ./cmd/veqri-build desktop

package-all:
	go run ./cmd/veqri-build all

release-check: build
	go run ./cmd/veqri-build --skip-npm-ci desktop
	go run ./scripts/release-smoke.go
	if [ "$$(go env GOOS)" = "darwin" ]; then \
		go run ./scripts/release-smoke.go --core build/release/Veqri.app/Contents/MacOS/veqri-desktop --core-arg=--veqri-managed-core; \
	else \
		go run ./scripts/release-smoke.go --core build/release/veqri-desktop --core-arg=--veqri-managed-core; \
	fi

test: test-go test-desktop test-android

test-go:
	go test -race ./...

test-integration:
	go test -race ./tests/integration/... ./tests/e2e/...

test-desktop:
	cd apps/desktop && npm ci && npm run typecheck && npm test -- --run && npm run build

test-android:
	cd apps/android && ./gradlew --no-daemon :protocol:checkAndroidProtocolBindings testDebugUnitTest lintDebug assembleDebug assembleRelease assembleDebugAndroidTest

lint:
	go vet ./...
	cd apps/desktop && npm run typecheck
	cd apps/android && ./gradlew --no-daemon :protocol:checkAndroidProtocolBindings lintDebug

run-core:
	go run ./cmd/veqri-core

run-desktop:
	cd apps/desktop && npm run dev

android-debug:
	go run ./cmd/veqri-build android

connector-simulators:
	./scripts/simulate-connectors.sh

clean:
	go clean -testcache
	rm -rf build apps/desktop/dist apps/desktop/node_modules apps/android/app/build apps/android/protocol/build
