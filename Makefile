BINARY := bin/kit
VERSION ?= dev
COMMIT ?= $(shell git rev-parse --short=12 HEAD 2>/dev/null || printf unknown)
BUILD_DATE ?= $(shell date -u +%Y-%m-%dT%H:%M:%SZ)
LDFLAGS := -s -w \
	-X kit/internal/buildinfo.Version=$(VERSION) \
	-X kit/internal/buildinfo.Commit=$(COMMIT) \
	-X kit/internal/buildinfo.BuildDate=$(BUILD_DATE)

.PHONY: build test check release

build:
	mkdir -p bin
	CGO_ENABLED=0 go build -trimpath -ldflags "$(LDFLAGS)" -o $(BINARY) ./cmd/kit

test:
	go test ./...

check:
	@test -z "$$(gofmt -l .)" || { echo "gofmt가 필요한 Go 파일이 있습니다." >&2; gofmt -l . >&2; exit 1; }
	go vet ./...
	go test ./...
	sh -n install.sh
	bash -n \
		deploy/activate.sh \
		deploy/generate-release-metadata.sh \
		deploy/rollback.sh \
		deploy/ssh-wrapper.sh \
		deploy/tests/ssh-wrapper.sh \
		deploy/tests/activate-integration.sh \
		deploy/apps-prod/deploy.sh \
		deploy/deploy-manual.sh \
		scripts/github-protection.sh
	python3 -m json.tool .github/protection/main.json >/dev/null
	python3 -m json.tool .github/protection/develop.json >/dev/null
	bash deploy/tests/ssh-wrapper.sh

release:
	@if [ "$(VERSION)" = "dev" ]; then \
		./deploy/deploy-manual.sh; \
	else \
		./deploy/deploy-manual.sh "$(VERSION)"; \
	fi
