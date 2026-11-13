#!/usr/bin/env bash
# Validate the agent work: what CI runs on a change, and the same thing from a shell.
#
#   scripts/ci-agent.sh              # build, format, vet, test and lint what the branch touches
#   scripts/ci-agent.sh go lint      # named stages only
#   scripts/ci-agent.sh e2e          # the agent browser suite, which needs the e2e toolchain
#   AGENT_CI_BASE=origin/main scripts/ci-agent.sh
set -uo pipefail

BASE=${AGENT_CI_BASE:-origin/main}
AGENT_PACKAGES=(./services/agent/... ./models/agent/... ./routers/web/agent/...)
LOG_DIR=${AGENT_CI_LOGS:-${TMPDIR:-/tmp}}
passed=0
failed=0

usage() {
	sed -n '2,8p' "$0" | sed 's/^# \{0,1\}//'
	exit "${1:-0}"
}

note() { printf '\n%s\n' "$1"; }

# check runs one command, keeps its output in a log and reports a failure as one line
check() {
	local name=$1
	shift
	local log="${LOG_DIR}/ci-agent-${name}.log"
	if "$@" >"$log" 2>&1; then
		printf '  ok   %-16s %s\n' "$name" "$(tail -1 "$log" | cut -c1-90)"
		passed=$((passed + 1))
		return
	fi
	printf '  FAIL %-16s %s\n' "$name" "$(grep -m1 -E 'FAIL|error|Error|issues|is not formatted' "$log" | cut -c1-120)"
	tail -25 "$log" | sed 's/^/       /'
	printf '       full log: %s\n' "$log"
	failed=$((failed + 1))
}

# what the branch changed, as go package paths, minus what needs an instance of its own
changed_packages() {
	git diff --name-only "$BASE"...HEAD -- '*.go' 2>/dev/null |
		sed -E 's#/[^/]+$##' | sort -u |
		grep -vE '^($|tests/|tools/|build/|assets/|web_src/|docs/|integrations/)' |
		sed 's#^#gitea.dev/#' | tr '\n' ' '
}

stage_go() {
	note 'go: it builds, it is formatted, it passes'
	check go.build go build -tags sqlite ./...
	check go.fmt bash -c '
		unformatted=$(gofmt -l $(git ls-files "*.go" | grep -vE "^(vendor|node_modules|public)/") | head -20)
		if [ -n "$unformatted" ]; then echo "$unformatted"; echo "is not formatted: gofmt -w them"; exit 1; fi
		echo "gofmt: clean"'
	check go.vet go vet "${AGENT_PACKAGES[@]}"
	check go.test.agent go test -tags sqlite -count=1 "${AGENT_PACKAGES[@]}"
	# the tests of what changed are upstream's to run: a few of them want a database of their
	# own or a running object store, so what changes here is held to a vet instead
	local packages
	packages=$(changed_packages)
	if [ -n "$packages" ]; then
		# shellcheck disable=SC2086
		check go.vet.changed go vet $packages
	else
		printf '  skip go.vet.changed\n'
	fi
}

stage_lint() {
	note 'lint: what the branch is held to'
	# the linter the Makefile pins; the workflow installs it beside go
	check lint.go golangci-lint run "${AGENT_PACKAGES[@]}"
	check lint.locale bash -c '
		jq -e . options/locale/locale_en-US.json >/dev/null || { echo "not valid json"; exit 1; }
		# one key per line, so a key written twice is seen here rather than dropped when it is read
		duplicates=$(grep -oE "^  \"[^\"]+\"" options/locale/locale_en-US.json | sort | uniq -d)
		if [ -n "$duplicates" ]; then echo "$duplicates"; echo "is written twice"; exit 1; fi
		echo "locale: $(jq length options/locale/locale_en-US.json) keys, no duplicates"'
	check lint.templates make lint-templates
	check lint.js make lint-js
}

# the browsers and their system libraries are installed before this, which needs root: CI does
# it, and a shell that already has them runs what is below as it stands
stage_e2e() {
	note 'e2e: the agent surfaces in a browser'
	check e2e.build make frontend backend
	check e2e.agent bash -c "EXECUTABLE=gitea ./tools/test-e2e.sh tests/e2e/agent.test.ts"
}

stages=("$@")
if [ ${#stages[@]} -eq 0 ]; then
	stages=(go lint)
fi
for stage in "${stages[@]}"; do
	case "$stage" in
	-h | --help) usage 0 ;;
	go | lint | e2e) ;;
	*)
		printf 'unknown stage %s\n' "$stage" >&2
		usage 2
		;;
	esac
done

for stage in "${stages[@]}"; do
	"stage_${stage}"
done

printf '\nci-agent: %s passed, %s failed\n' "$passed" "$failed"
[ "$failed" -eq 0 ] || exit 1
