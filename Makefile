MODULE := github.com/sealbro/go-discord-caller

# Packages included in coverage: everything except cmd/ and integration/.
COVER_PKGS := $(shell go list ./... | grep -v -E '^$(MODULE)/(cmd|integration)(/|$$)' | grep -v '$(MODULE)/internal/telemetry')
COVER_PKGS_CSV := $(shell echo $(COVER_PKGS) | tr ' ' ',')

STRESS_DURATION ?=
export STRESS_DURATION
STRESS_TIMEOUT := $(if $(STRESS_DURATION),$(STRESS_DURATION)10m,60m)
STRESS_FLAGS := --tags=stress -v -count=1 -timeout $(STRESS_TIMEOUT) ./integration/

.PHONY: test test-unit test-integration coverage clean-cache \
	test-stress test-stress-audio test-stress-star test-stress-mixminus

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

test-stress-audio:
	go test $(STRESS_FLAGS) -run TestStress_AllBotsPlayAudio

test-stress-star:
	go test $(STRESS_FLAGS) -run TestStress_OneManyStarTopologyLong

test-stress-mixminus:
	go test $(STRESS_FLAGS) -run TestStress_GuildCallerMixMinusLong

test-stress:
	go test $(STRESS_FLAGS)

test: test-unit test-integration
	@echo "mode: atomic" > combined.out
	@grep -h -v "^mode:" unit.out coverage.out >> combined.out
	go tool cover -func=combined.out
