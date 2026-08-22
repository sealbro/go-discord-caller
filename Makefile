MODULE := github.com/sealbro/go-discord-caller

# Packages included in coverage: everything except cmd/ and integration/.
COVER_PKGS := $(shell go list ./... | grep -v -E '^$(MODULE)/(cmd|integration)(/|$$)' | grep -v '$(MODULE)/internal/telemetry')
COVER_PKGS_CSV := $(shell echo $(COVER_PKGS) | tr ' ' ',')

.PHONY: test test-unit test-integration coverage clean-cache

clean-cache:
	go clean -testcache

test-unit:
	go test --race -covermode=atomic -coverprofile=unit.out \
		-coverpkg=$(COVER_PKGS_CSV) \
		./internal/...

test-integration:
	go test --tags=integration --timeout=15m -covermode=atomic -coverprofile=coverage.out \
		-coverpkg=$(COVER_PKGS_CSV) \
		./integration/...

test: test-unit test-integration
	@echo "mode: atomic" > combined.out
	@grep -h -v "^mode:" unit.out coverage.out >> combined.out
	go tool cover -func=combined.out
