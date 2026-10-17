# SPDX-License-Identifier: MIT
# Copyright The Muster Authors

.DEFAULT_GOAL := help

GO ?= go
DOCKER ?= docker
CURL ?= curl
MODULE := github.com/muster-io/muster-bridge-go
BIN := bin
TOOLS := $(BIN)/tools

# Evaluated only when a tool is built, so that targets without Go, such as spec-check, need no Go.
GO_TOOLCHAIN = $(shell $(GO) env GOVERSION)
GOLANGCI_LINT_PKG := github.com/golangci/golangci-lint/v2/cmd/golangci-lint
# renovate: datasource=go depName=github.com/golangci/golangci-lint/v2
GOLANGCI_LINT_VERSION := v2.14.0
GOLANGCI_LINT := $(TOOLS)/golangci-lint-$(GOLANGCI_LINT_VERSION)
GO_LICENSES_PKG := github.com/google/go-licenses/v2
# renovate: datasource=go depName=github.com/google/go-licenses/v2
GO_LICENSES_VERSION := v2.0.1
GO_LICENSES := $(TOOLS)/go-licenses-$(GO_LICENSES_VERSION)
GOVULNCHECK_PKG := golang.org/x/vuln/cmd/govulncheck
# renovate: datasource=go depName=golang.org/x/vuln
GOVULNCHECK_VERSION := v1.8.0
GOVULNCHECK := $(TOOLS)/govulncheck-$(GOVULNCHECK_VERSION)
OAPI_CODEGEN_PKG := github.com/oapi-codegen/oapi-codegen/v2/cmd/oapi-codegen
# renovate: datasource=go depName=github.com/oapi-codegen/oapi-codegen/v2
OAPI_CODEGEN_VERSION := v2.8.0
OAPI_CODEGEN := $(TOOLS)/oapi-codegen-$(OAPI_CODEGEN_VERSION)

MUSTER_SPEC_URL ?= https://raw.githubusercontent.com/muster-io/muster/master/api/bridge.yaml
MUSTER_IMAGE ?= ghcr.io/muster-io/muster:edge
ALLOWED_LICENSES := MIT,MIT-0,BSD-2-Clause,BSD-3-Clause,Apache-2.0,ISC,0BSD,Unlicense,CC0-1.0
COVERAGE_MIN := 90
COVER_PROFILE := $(BIN)/cover.out

EXAMPLE_LISTEN := 127.0.0.1:18095
PUSH_LISTEN := 127.0.0.1:18096
CONFORMANCE_DIR := $(BIN)/conformance
CHECK_FLAGS := --token-file /token --channel alerts --forbidden-channel nobot --handle alice
STEPS := info channel_check post replayed_key buttons edit thread_reply thread_ref forbidden_channel user_lookup \
	direct_message delete poll press push answer
RUN1_SKIP := press push
RUN2_SKIP := buttons user_lookup direct_message delete press push
RUN1_PASS := $(filter-out $(RUN1_SKIP),$(STEPS))
RUN2_PASS := $(filter-out $(RUN2_SKIP),$(STEPS))

.PHONY: help fmt lint test generate generate-check spec-check spec-sync licenses vulncheck build ci conformance clean

help: ## List the targets
	@awk 'BEGIN { FS = ":.*## " } /^[a-z][a-z-]*:.*## / { printf "  %-16s %s\n", $$1, $$2 }' $(MAKEFILE_LIST)

fmt: $(GOLANGCI_LINT) ## Format the Go code (gofmt and goimports through golangci-lint)
	$(GOLANGCI_LINT) fmt

lint: $(GOLANGCI_LINT) ## Check the licence headers, go.mod and go.sum, and the code with golangci-lint
	@failed=0; \
	sh -c "$$HEADER_CHECK_SH" || failed=1; \
	$(GO) mod tidy -diff || failed=1; \
	$(GOLANGCI_LINT) run ./... || failed=1; \
	exit $$failed

test: ## Run the tests with the race detector and the coverage gate of the package (models.gen.go excluded)
	@mkdir -p $(BIN)
	$(GO) test -race -covermode=atomic -coverprofile=$(COVER_PROFILE) .
	@grep -v '^$(MODULE)/models\.gen\.go:' $(COVER_PROFILE) > $(BIN)/cover.lib.out
	@$(GO) tool cover -func=$(BIN)/cover.lib.out | awk -v min=$(COVERAGE_MIN) '$$1 == "total:" { \
		sub("%", "", $$3); \
		printf "coverage of the package, models.gen.go excluded: %s%% (minimum %s%%)\n", $$3, min; \
		if ($$3 + 0 < min) exit 1 }'

generate: $(OAPI_CODEGEN) ## Generate models.gen.go from api/bridge.yaml
	$(OAPI_CODEGEN) -config api/codegen.yaml api/bridge.yaml
	@awk '/^\/\/ Package bridge provides / { skip = 1; next } \
		skip && /^\/\/$$/ { skip = 0; next } \
		{ print } \
		/^\/\/ Code generated .* DO NOT EDIT\.$$/ { print "" }' models.gen.go > models.gen.go.tmp && \
		mv models.gen.go.tmp models.gen.go
	@if grep -q '^// Package ' models.gen.go; then \
		echo "models.gen.go still has a package comment: the generator's template changed, fix the awk of generate" >&2; \
		exit 1; \
	fi

generate-check: ## Fail when models.gen.go is not what make generate makes of api/bridge.yaml
	@tmp=$$(mktemp); trap 'rm -f "$$tmp"' EXIT; \
	cp models.gen.go "$$tmp" 2>/dev/null || : > "$$tmp"; \
	$(MAKE) --no-print-directory generate || exit 1; \
	if cmp -s "$$tmp" models.gen.go; then \
		echo "generated files are current"; \
	else \
		printf 'stale generated files:\n  models.gen.go\nrun make generate and commit the result\n' >&2; \
		exit 1; \
	fi

spec-check: ## Compare api/bridge.yaml byte for byte with the one on Muster's master (MUSTER_SPEC_URL)
	@tmp=$$(mktemp); trap 'rm -f "$$tmp"' EXIT; \
	if ! $(CURL) -fsSL --retry 3 "$(MUSTER_SPEC_URL)" -o "$$tmp"; then \
		echo "cannot download $(MUSTER_SPEC_URL)" >&2; \
		exit 1; \
	fi; \
	if cmp -s "$$tmp" api/bridge.yaml; then \
		echo "api/bridge.yaml is the copy of $(MUSTER_SPEC_URL)"; \
		exit 0; \
	fi; \
	diff -u --label "$(MUSTER_SPEC_URL)" --label api/bridge.yaml "$$tmp" api/bridge.yaml; \
	echo "api/bridge.yaml differs from $(MUSTER_SPEC_URL): run make spec-sync" >&2; \
	exit 1

spec-sync: ## Replace api/bridge.yaml with the one on Muster's master (MUSTER_SPEC_URL) and regenerate
	@tmp=$$(mktemp); \
	if ! $(CURL) -fsSL --retry 3 "$(MUSTER_SPEC_URL)" -o "$$tmp"; then \
		rm -f "$$tmp"; \
		echo "cannot download $(MUSTER_SPEC_URL)" >&2; \
		exit 1; \
	fi; \
	chmod 0644 "$$tmp" && mkdir -p api && mv "$$tmp" api/bridge.yaml
	@$(MAKE) --no-print-directory generate

licenses: $(GO_LICENSES) ## Fail on a linked dependency whose licence is not in ALLOWED_LICENSES
	$(GO_LICENSES) check ./... --allowed_licenses=$(ALLOWED_LICENSES)

vulncheck: $(GOVULNCHECK) ## Find vulnerable Go code that is called (govulncheck)
	$(GOVULNCHECK) ./...

build: ## Build the package, and the minimal example into bin/minimal
	$(GO) build ./...
	$(GO) build -o $(BIN)/minimal ./examples/minimal

ci: lint generate-check spec-check licenses test build ## The checks of a pull request, locally (vulncheck and conformance run in their own CI jobs)

conformance: ## Run muster bridge check from MUSTER_IMAGE against examples/minimal three times (Linux, Docker)
	@sh -c "$$CONFORMANCE_SH"

clean: ## Remove bin/: the example, the coverage profiles and the installed tools
	rm -rf $(BIN)

$(GOLANGCI_LINT):
	$(call go-install,$(GOLANGCI_LINT_PKG),$(GOLANGCI_LINT_VERSION),$@)

$(GO_LICENSES):
	$(call go-install,$(GO_LICENSES_PKG),$(GO_LICENSES_VERSION),$@)

$(GOVULNCHECK):
	$(call go-install,$(GOVULNCHECK_PKG),$(GOVULNCHECK_VERSION),$@)

$(OAPI_CODEGEN):
	$(call go-install,$(OAPI_CODEGEN_PKG),$(OAPI_CODEGEN_VERSION),$@)

# go-install builds the tool $(1) at the version $(2) with the project's Go toolchain into the file $(3), trying three
# times, since a module download can fail for a moment.
define go-install
	@mkdir -p $(TOOLS)
	@tmp=$$(mktemp -d); trap 'rm -rf "$$tmp"' EXIT; \
	for n in 1 2 3; do \
		if GOTOOLCHAIN=$(GO_TOOLCHAIN) GOBIN="$$tmp" $(GO) install $(1)@$(2); then break; fi; \
		if [ "$$n" = 3 ]; then echo "cannot install $(1)@$(2)" >&2; exit 1; fi; \
		sleep $$((n * 10)); \
	done; \
	mv "$$tmp"/* $(3)
endef

# The licence header check of lint: every Go file, workflow, YAML file, Renovate configuration and the Makefile start
# with the MIT header; generated files and the copy api/bridge.yaml are exempt.
define HEADER_CHECK_SH
missing=$$(find . \( -path ./bin -o -path ./.git \) -prune -o -type f \
	\( -name '*.go' -o -name '*.yml' -o -name '*.yaml' -o -name '*.json5' -o -name Makefile \) -print | sort |
	while IFS= read -r f; do
		[ "$$f" = ./api/bridge.yaml ] && continue
		head -n 5 "$$f" | grep -q 'Code generated .* DO NOT EDIT\.' && continue
		if ! head -n 5 "$$f" | grep -q 'SPDX-License-Identifier: MIT' ||
			! head -n 5 "$$f" | grep -q 'Copyright The Muster Authors'; then
			echo "$$f"
		fi
	done)
if [ -n "$$missing" ]; then
	echo "files without the licence header (SPDX-License-Identifier: MIT, Copyright The Muster Authors):"
	echo "$$missing" | sed 's/^/  /'
	exit 1
fi
endef
export HEADER_CHECK_SH

# The conformance check: examples/minimal against muster bridge check of MUSTER_IMAGE, three runs, each with the steps
# it must pass and skip. Docker's host network makes it Linux only.
define CONFORMANCE_SH
unset BRIDGE_FEATURES
dir=$(abspath $(CONFORMANCE_DIR))
rm -rf "$$dir" && mkdir -p "$$dir" && chmod 0755 "$$dir" || exit 1
failed=0
pid=
check=
cleanup() {
	if [ -n "$$check" ]; then kill "$$check" 2>/dev/null; wait "$$check" 2>/dev/null; fi
	if [ -n "$$pid" ]; then kill "$$pid" 2>/dev/null; wait "$$pid" 2>/dev/null; fi
}
trap cleanup EXIT
trap 'exit 130' INT TERM

$(GO) build -o $(abspath $(BIN))/minimal ./examples/minimal || exit 1
$(DOCKER) pull --quiet $(MUSTER_IMAGE) || exit 1
echo "muster image: $$($(DOCKER) image inspect --format '{{index .RepoDigests 0}}' $(MUSTER_IMAGE))"
$(DOCKER) run --rm $(MUSTER_IMAGE) version || exit 1

printf 'muster-bridge-go-%s\n' "$$(od -An -N16 -tx1 /dev/urandom | tr -d ' \n')" > "$$dir/token"
printf 'whsec_%s\n' "$$(head -c 32 /dev/urandom | base64 | tr -d '\n')" > "$$dir/push-secret"
chmod 0644 "$$dir/token" "$$dir/push-secret"

# start <name> <features|all> [flags]: start the example and wait until it answers.
start() {
	name=$$1
	features=$$2
	shift 2
	if [ "$$features" = all ]; then
		$(abspath $(BIN))/minimal -token-file "$$dir/token" -listen $(EXAMPLE_LISTEN) "$$@" > "$$dir/$$name-example.log" 2>&1 &
	else
		BRIDGE_FEATURES="$$features" $(abspath $(BIN))/minimal -token-file "$$dir/token" -listen $(EXAMPLE_LISTEN) "$$@" \
			> "$$dir/$$name-example.log" 2>&1 &
	fi
	pid=$$!
	n=0
	while [ "$$($(CURL) -s -o /dev/null -w '%{http_code}' http://$(EXAMPLE_LISTEN)/v1/info)" != 401 ]; do
		n=$$((n + 1))
		if [ "$$n" -ge 100 ]; then
			echo "conformance $$name: the example did not start"
			cat "$$dir/$$name-example.log"
			return 1
		fi
		sleep 0.1
	done
}

stop() {
	if [ -n "$$pid" ]; then kill "$$pid" 2>/dev/null; wait "$$pid" 2>/dev/null; fi
	pid=
}

sorted() { printf '%s\n' $$1 | sed '/^$$/d' | sort | tr '\n' ' '; }

# verify <name> <status> <pass> <skip>: compare the check's lines with the steps it must pass and skip.
verify() {
	name=$$1
	status=$$2
	gotpass=$$(sorted "$$(sed -n 's/^PASS \([a-z_]*\)$$/\1/p' "$$dir/$$name.out")")
	gotskip=$$(sorted "$$(sed -n 's/^SKIP \([a-z_]*\): .*$$/\1/p' "$$dir/$$name.out")")
	wantpass=$$(sorted "$$3")
	wantskip=$$(sorted "$$4")
	if [ "$$status" -ne 0 ] || grep -q '^FAIL ' "$$dir/$$name.out" ||
		[ "$$gotpass" != "$$wantpass" ] || [ "$$gotskip" != "$$wantskip" ]; then
		echo "conformance $$name: exit $$status; PASS [$$gotpass] want [$$wantpass]; SKIP [$$gotskip] want [$$wantskip]"
		echo "--- muster bridge check, stderr:"
		cat "$$dir/$$name.err"
		echo "--- the example's log:"
		cat "$$dir/$$name-example.log"
		failed=1
	else
		echo "conformance $$name: passed"
	fi
}

echo "== run 1: every feature, --no-interactive"
if start all all; then
	$(DOCKER) run --rm --network host -v "$$dir/token:/token:ro" $(MUSTER_IMAGE) bridge check \
		http://$(EXAMPLE_LISTEN) $(CHECK_FLAGS) --no-interactive > "$$dir/all.out" 2> "$$dir/all.err"
	status=$$?
	cat "$$dir/all.out"
	verify all "$$status" "$(RUN1_PASS)" "$(RUN1_SKIP)"
else
	failed=1
fi
stop

echo "== run 2: BRIDGE_FEATURES=thread_messages, --no-interactive"
if start thread-messages thread_messages; then
	$(DOCKER) run --rm --network host -v "$$dir/token:/token:ro" $(MUSTER_IMAGE) bridge check \
		http://$(EXAMPLE_LISTEN) $(CHECK_FLAGS) --no-interactive > "$$dir/thread-messages.out" 2> "$$dir/thread-messages.err"
	status=$$?
	cat "$$dir/thread-messages.out"
	verify thread-messages "$$status" "$(RUN2_PASS)" "$(RUN2_SKIP)"
else
	failed=1
fi
stop

echo "== run 3: every feature, interactive, the example pushing to the check's listener"
if start interactive all -push-url http://$(PUSH_LISTEN)/ -push-secret-file "$$dir/push-secret"; then
	$(DOCKER) run --rm --network host -v "$$dir/token:/token:ro" -v "$$dir/push-secret:/push-secret:ro" \
		$(MUSTER_IMAGE) bridge check http://$(EXAMPLE_LISTEN) $(CHECK_FLAGS) \
		--push-listen $(PUSH_LISTEN) --push-secret-file /push-secret > "$$dir/interactive.out" 2> "$$dir/interactive.err" &
	check=$$!
	id=
	n=0
	while [ "$$n" -lt 1200 ]; do
		id=$$(sed -n 's/^WAIT press: .*, message_id \(.*\)$$/\1/p' "$$dir/interactive.out")
		if [ -n "$$id" ] || ! kill -0 "$$check" 2>/dev/null; then break; fi
		n=$$((n + 1))
		sleep 0.1
	done
	if [ -n "$$id" ]; then
		$(CURL) -fsS -X POST -H 'Content-Type: application/json' \
			-d "{\"message_id\":\"$$id\",\"user\":\"alice\"}" http://$(EXAMPLE_LISTEN)/demo/press
	else
		echo "conformance interactive: no WAIT press line"
	fi
	wait "$$check"
	status=$$?
	check=
	cat "$$dir/interactive.out"
	verify interactive "$$status" "$(STEPS)" ""
else
	failed=1
fi
stop

exit $$failed
endef
export CONFORMANCE_SH
