MODULE := github.com/sealbro/go-discord-caller

# Packages included in coverage: everything except cmd/ and integration/.
COVER_PKGS := $(shell go list ./... | grep -v -E '^$(MODULE)/(cmd|integration)(/|$$)' | grep -v '$(MODULE)/internal/telemetry')
COVER_PKGS_CSV := $(shell echo $(COVER_PKGS) | tr ' ' ',')

STRESS_DURATION ?=
export STRESS_DURATION
STRESS_TIMEOUT := $(if $(STRESS_DURATION),$(STRESS_DURATION)10m,60m)
STRESS_FLAGS := --tags=stress -v -count=1 -timeout $(STRESS_TIMEOUT) ./integration/

.PHONY: test test-unit test-integration coverage clean-cache \
	test-stress test-stress-audio test-stress-star test-stress-mixminus \
	release

clean-cache:
	go clean -testcache

RELEASE_VERSION := $(shell grep -m1 -oE '^\#\# \[[0-9]+\.[0-9]+\.[0-9]+\]' CHANGELOG.md | grep -oE '[0-9]+\.[0-9]+\.[0-9]+')

release:
	@if [ -z "$(RELEASE_VERSION)" ]; then \
		echo "Could not find a version heading in CHANGELOG.md"; exit 1; \
	fi
	@if git rev-parse "$(RELEASE_VERSION)" >/dev/null 2>&1; then \
		echo "Tag $(RELEASE_VERSION) already exists"; exit 1; \
	fi
	@read -p "Do you want to make this $(RELEASE_VERSION) release? [y/N] " ans; \
	case "$$ans" in \
		[yY]) ;; \
		*) echo "Aborted"; exit 1;; \
	esac
	git tag $(RELEASE_VERSION)
	git push origin $(RELEASE_VERSION)

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
