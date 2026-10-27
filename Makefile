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
GOLANGCI_LINT_VERSION := v2.15.0
GOLANGCI_LINT := $(TOOLS)/golangci-lint-$(GOLANGCI_LINT_VERSION)
GO_LICENSES_PKG := github.com/google/go-licenses/v2
# renovate: datasource=go depName=github.com/google/go-licenses/v2
GO_LICENSES_VERSION := v2.0.1
GO_LICENSES := $(TOOLS)/go-licenses-$(GO_LICENSES_VERSION)
GOVULNCHECK_PKG := golang.org/x/vuln/cmd/govulncheck
# renovate: datasource=go depName=golang.org/x/vuln
GOVULNCHECK_VERSION := v1.9.0
GOVULNCHECK := $(TOOLS)/govulncheck-$(GOVULNCHECK_VERSION)
OAPI_CODEGEN_PKG := github.com/oapi-codegen/oapi-codegen/v2/cmd/oapi-codegen
# renovate: datasource=go depName=github.com/oapi-codegen/oapi-codegen/v2
OAPI_CODEGEN_VERSION := v2.8.1
OAPI_CODEGEN := $(TOOLS)/oapi-codegen-$(OAPI_CODEGEN_VERSION)

MUSTER_SPEC_URL ?= https://raw.githubusercontent.com/muster-io/muster/master/api/bridge.yaml
MUSTER_IMAGE ?= ghcr.io/muster-io/muster:edge
IMAGE ?= muster-example-bridge:dev
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

.PHONY: help fmt lint test generate generate-check spec-check spec-sync licenses vulncheck build ci image conformance \
	clean

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

test: ## Run the tests of the package and the examples with the race detector, and the package's coverage gate
	@mkdir -p $(BIN)
	$(GO) test -race -covermode=atomic -coverprofile=$(COVER_PROFILE) .
	@grep -v '^$(MODULE)/models\.gen\.go:' $(COVER_PROFILE) > $(BIN)/cover.lib.out
	@$(GO) tool cover -func=$(BIN)/cover.lib.out | awk -v min=$(COVERAGE_MIN) '$$1 == "total:" { \
		sub("%", "", $$3); \
		printf "coverage of the package, models.gen.go excluded: %s%% (minimum %s%%)\n", $$3, min; \
		if ($$3 + 0 < min) exit 1 }'
	$(GO) test -race ./examples/...

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

build: ## Build the package, and the examples into bin/minimal and bin/chat
	$(GO) build ./...
	$(GO) build -o $(BIN)/minimal ./examples/minimal
	$(GO) build -o $(BIN)/chat ./examples/chat

ci: lint generate-check spec-check licenses test build ## The checks of a pull request, locally (vulncheck, image and conformance run in their own CI jobs)

image: ## Build the example bridge's image as IMAGE for this machine's platform (Docker) and print its -help
	$(DOCKER) build -f examples/chat/Dockerfile -t $(IMAGE) .
	$(DOCKER) run --rm $(IMAGE) -help

conformance: ## Run muster bridge check from MUSTER_IMAGE against both examples, three times each (Linux, Docker)
	@sh -c "$$CONFORMANCE_SH"

clean: ## Remove bin/: the examples, the coverage profiles and the installed tools
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

# The licence header check of lint: every Go file, workflow, YAML file, Renovate configuration, the Makefile, the
# Dockerfile, .dockerignore and the page's HTML, JavaScript and CSS start with the MIT header; generated files and the
# copy api/bridge.yaml are exempt.
define HEADER_CHECK_SH
missing=$$(find . \( -path ./bin -o -path ./.git \) -prune -o -type f \
	\( -name '*.go' -o -name '*.yml' -o -name '*.yaml' -o -name '*.json5' -o -name Makefile -o -name Dockerfile \
	-o -name .dockerignore -o -name '*.html' -o -name '*.js' -o -name '*.css' \) -print | sort |
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

# The conformance check: examples/minimal and examples/chat against muster bridge check of MUSTER_IMAGE, three runs
# each, every run with the steps it must pass and skip. Docker's host network makes it Linux only.
define CONFORMANCE_SH
unset BRIDGE_TOKEN BRIDGE_PUSH_URL BRIDGE_PUSH_SECRET BRIDGE_FEATURES
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

minimal=$(abspath $(BIN))/minimal
chat=$(abspath $(BIN))/chat
$(GO) build -o "$$minimal" ./examples/minimal || exit 1
$(GO) build -o "$$chat" ./examples/chat || exit 1
$(DOCKER) pull --quiet $(MUSTER_IMAGE) || exit 1
echo "muster image: $$($(DOCKER) image inspect --format '{{index .RepoDigests 0}}' $(MUSTER_IMAGE))"
$(DOCKER) run --rm $(MUSTER_IMAGE) version || exit 1

printf 'muster-bridge-go-%s\n' "$$(od -An -N16 -tx1 /dev/urandom | tr -d ' \n')" > "$$dir/token"
printf 'whsec_%s\n' "$$(head -c 32 /dev/urandom | base64 | tr -d '\n')" > "$$dir/push-secret"
chmod 0644 "$$dir/token" "$$dir/push-secret"

# start <name> <command…>: start an example — its binary with flags, after env and variables if any — with the token
# file and the listen address appended, and wait until it answers.
start() {
	name=$$1
	shift
	"$$@" -token-file "$$dir/token" -listen $(EXAMPLE_LISTEN) > "$$dir/$$name-example.log" 2>&1 &
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

# unattended <name> <pass> <skip>: run the check with --no-interactive and verify its lines.
unattended() {
	$(DOCKER) run --rm --network host -v "$$dir/token:/token:ro" $(MUSTER_IMAGE) bridge check \
		http://$(EXAMPLE_LISTEN) $(CHECK_FLAGS) --no-interactive > "$$dir/$$1.out" 2> "$$dir/$$1.err"
	status=$$?
	cat "$$dir/$$1.out"
	verify "$$1" "$$status" "$$2" "$$3"
}

# interactive <name> <press URL> <press body>: run the check with a person and its push listener; the person presses
# the message that the WAIT press line names, as alice, by posting the body, a printf format whose %s is the message
# id, to the press URL. Every step must pass.
interactive() {
	$(DOCKER) run --rm --network host -v "$$dir/token:/token:ro" -v "$$dir/push-secret:/push-secret:ro" \
		$(MUSTER_IMAGE) bridge check http://$(EXAMPLE_LISTEN) $(CHECK_FLAGS) \
		--push-listen $(PUSH_LISTEN) --push-secret-file /push-secret > "$$dir/$$1.out" 2> "$$dir/$$1.err" &
	check=$$!
	id=
	n=0
	while [ "$$n" -lt 1200 ]; do
		id=$$(sed -n 's/^WAIT press: .*, message_id \(.*\)$$/\1/p' "$$dir/$$1.out")
		if [ -n "$$id" ] || ! kill -0 "$$check" 2>/dev/null; then break; fi
		n=$$((n + 1))
		sleep 0.1
	done
	if [ -n "$$id" ]; then
		code=$$($(CURL) -sS -X POST -H 'Content-Type: application/json' -d "$$(printf "$$3" "$$id")" \
			-o "$$dir/$$1.press" -w '%{http_code}' "$$2")
		case $$code in
		2??) ;;
		*) echo "conformance $$1: the press answered $$code: $$(cat "$$dir/$$1.press" 2>/dev/null)" ;;
		esac
	else
		echo "conformance $$1: no WAIT press line"
	fi
	wait "$$check"
	status=$$?
	check=
	cat "$$dir/$$1.out"
	verify "$$1" "$$status" "$(STEPS)" ""
}

echo "== minimal 1: every feature, --no-interactive"
if start minimal-all "$$minimal"; then
	unattended minimal-all "$(RUN1_PASS)" "$(RUN1_SKIP)"
else
	failed=1
fi
stop

echo "== minimal 2: BRIDGE_FEATURES=thread_messages, --no-interactive"
if start minimal-thread-messages env BRIDGE_FEATURES=thread_messages "$$minimal"; then
	unattended minimal-thread-messages "$(RUN2_PASS)" "$(RUN2_SKIP)"
else
	failed=1
fi
stop

echo "== minimal 3: every feature, interactive, the example pushing to the check's listener"
if start minimal-interactive "$$minimal" -push-url http://$(PUSH_LISTEN)/ -push-secret-file "$$dir/push-secret"; then
	interactive minimal-interactive http://$(EXAMPLE_LISTEN)/demo/press '{"message_id":"%s","user":"alice"}'
else
	failed=1
fi
stop

echo "== chat 1: every feature, --no-interactive"
if start chat-all "$$chat"; then
	unattended chat-all "$(RUN1_PASS)" "$(RUN1_SKIP)"
else
	failed=1
fi
stop

echo "== chat 2: BRIDGE_FEATURES=thread_messages, --no-interactive"
if start chat-thread-messages env BRIDGE_FEATURES=thread_messages "$$chat"; then
	unattended chat-thread-messages "$(RUN2_PASS)" "$(RUN2_SKIP)"
else
	failed=1
fi
stop

echo "== chat 3: every feature, interactive, the example pushing to the check's listener"
if start chat-interactive env BRIDGE_PUSH_URL=http://$(PUSH_LISTEN)/ "$$chat" \
	-push-secret-file "$$dir/push-secret"; then
	interactive chat-interactive http://$(EXAMPLE_LISTEN)/chat/api/press \
		'{"message_id":"%s","user":"alice","button":0}'
else
	failed=1
fi
stop

exit $$failed
endef
export CONFORMANCE_SH
