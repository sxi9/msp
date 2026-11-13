# MSP phase 1 build. Every target mirrors the BUILD_AND_TEST section of docs/PLAN.md.
# CGO is never needed: modernc.org/sqlite is pure Go, so every binary is static.

VERSION      ?= $(shell cat VERSION)
LDFLAGS      := -s -w -X main.version=$(VERSION)
ANDROID_HOME ?= /opt/android-sdk
GRADLE_FLAGS ?= --no-daemon --console=plain --max-workers=1
GO           ?= go

export CGO_ENABLED = 0

.PHONY: build test race web agents android android-release android-test e2e compose-check docker clean

## build: server and desktop agent for this host into bin/
build:
	$(GO) build -ldflags "$(LDFLAGS)" -o bin/msp-server ./cmd/msp-server
	$(GO) build -ldflags "$(LDFLAGS)" -o bin/msp-agent  ./cmd/msp-agent

## test: vet + unit tests + in-process e2e (internal/e2e)
test:
	$(GO) vet ./...
	$(GO) test ./... -count=1 -p 4

## race: race detector on the concurrent packages (-race needs cgo, so the global CGO_ENABLED=0 is overridden here only)
race: export CGO_ENABLED = 1
race:
	$(GO) test -race ./internal/server ./internal/agent ./internal/e2e -count=1

## web: dashboard bundle (web/dist/app.js + app.css), then rebuild the server so go:embed picks it up
web:
	cd web && npm ci
	cd web && npm test
	cd web && npm run typecheck
	cd web && npm run build
	$(MAKE) build

## agents: cross-compile matrix into dist/ + SHA256SUMS + install scripts (scripts/build-agents.sh)
agents:
	VERSION=$(VERSION) scripts/build-agents.sh

## android: debug APK at android/app/build/outputs/apk/debug/app-debug.apk (lab use only: cleartext allowed, debuggable, debug-signed; never published)
android:
	cd android && ANDROID_HOME=$(ANDROID_HOME) ANDROID_SDK_ROOT=$(ANDROID_HOME) gradle $(GRADLE_FLAGS) :app:assembleDebug

## android-release: the publishable APK at android/app/build/outputs/apk/release/app-release.apk; needs MSP_ANDROID_KEYSTORE (+ _PASSWORD, optional _KEY_ALIAS/_KEY_PASSWORD), see docs/OPS.md section 3
android-release:
	@test -n "$$MSP_ANDROID_KEYSTORE" || { echo "MSP_ANDROID_KEYSTORE is not set: the release APK would be unsigned (docs/OPS.md section 3)"; exit 1; }
	cd android && ANDROID_HOME=$(ANDROID_HOME) ANDROID_SDK_ROOT=$(ANDROID_HOME) gradle $(GRADLE_FLAGS) :app:assembleRelease

## android-test: JVM protocol tests against internal/protocol/fixtures
android-test:
	cd android && ANDROID_HOME=$(ANDROID_HOME) ANDROID_SDK_ROOT=$(ANDROID_HOME) gradle $(GRADLE_FLAGS) :app:testDebugUnitTest

## e2e: real-binary end-to-end proof; exit 0 and "E2E OK" are the verdict
e2e: build
	scripts/e2e.sh

## compose-check: validate compose file and shell scripts without a Docker daemon
compose-check:
	docker compose -f deploy/docker-compose.yml config > /dev/null
	bash -n deploy/install.sh scripts/e2e.sh scripts/build-agents.sh

## docker: build the server image (needs a Docker daemon; CI's job)
docker:
	docker build -f deploy/Dockerfile -t msp-server:$(VERSION) -t msp-server:dev .

## clean: remove build outputs (keeps web/dist/index.html)
clean:
	rm -rf bin dist web/dist/app.js web/dist/app.css
