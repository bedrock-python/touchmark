.PHONY: install fmt check test-unit test-e2e test build snapshot docs-serve docs-build clean

# The targets of the other bedrock-python repositories, for Go. They run on
# Linux and macOS, and in Git Bash on Windows.
SHELL := bash
.SHELLFLAGS := -eu -o pipefail -c

# The tools run at pinned versions, never from go.mod: the module depends on
# two libraries and nothing else. The lint and govulncheck jobs of
# .github/workflows/ci.yml pin the same versions.
GOLANGCI_LINT_VERSION := v2.14.0
GOVULNCHECK_VERSION := v1.8.0
# The goreleaser of publish.yml, in its image as docs/project/release.md runs
# it. The image brings its own Go (1.27 in v2.18.2), where publish.yml builds
# with 1.26: a snapshot checks the configuration and the files, not the bytes
# of a release.
GORELEASER_IMAGE := goreleaser/goreleaser:v2.18.2@sha256:7077423cf5ef643ff56a34b58f93c1364e927e5c3dfa470eeabc44cab1a9c72b

# A golangci-lint on PATH runs when it is the pinned version; otherwise go run
# builds that version, once: the build cache keeps it.
GOLANGCI_LINT = $(if $(filter $(GOLANGCI_LINT_VERSION:v%=%),$(shell golangci-lint version --short 2>/dev/null)),golangci-lint,go run github.com/golangci/golangci-lint/v2/cmd/golangci-lint@$(GOLANGCI_LINT_VERSION))
GOVULNCHECK = go run golang.org/x/vuln/cmd/govulncheck@$(GOVULNCHECK_VERSION)

# goreleaser in its image, as the user who runs make on Linux so that dist/
# stays theirs (Docker Desktop maps the owner itself). MSYS_NO_PATHCONV keeps
# Git Bash from rewriting the container path /src, for this command only:
# set for git, it breaks file:// remotes on Windows. A goreleaser on PATH:
# make snapshot GORELEASER=goreleaser
GORELEASER = MSYS_NO_PATHCONV=1 docker run --rm -v "$(CURDIR):/src" -w /src $(if $(filter Linux,$(shell uname -s)),--user "$$(id -u):$$(id -g)" -e HOME=/tmp) $(GORELEASER_IMAGE)

# The race detector needs cgo, which Go turns off where it finds no C
# compiler; make test then runs without it.
RACE = $(if $(filter 1,$(shell go env CGO_ENABLED)),-race)

# The coverage gate of make test, which CI runs on Linux with the newest Go:
# just below the 88.0% the suite reached on Linux with git 2.47 on
# 2026-10-03. It counts every statement of the module, whichever package's
# tests run it. Where git is older than 2.45, or outside Linux, tests skip
# and the number comes out lower: run the suite in Docker (CONTRIBUTING.md).
COVERAGE_MIN := 87.5

# The Gitea of make test-e2e; `bash scripts/e2e/gitea.sh all` runs every
# supported Gitea and Forgejo.
E2E_IMAGE := docker.gitea.com/gitea:1.27.3

install:
	go mod download
	uv sync --no-dev --group docs

fmt:
	gofmt -w .

check:
	@unformatted="$$(gofmt -l .)"; \
	if [ -n "$$unformatted" ]; then echo "gofmt -l reports unformatted files:" >&2; echo "$$unformatted" >&2; exit 1; fi
	go vet ./...
	go vet -tags e2e ./...
	$(GOLANGCI_LINT) run
	$(GOVULNCHECK) ./...

test-unit:
	go test ./...

test-e2e:
	bash scripts/e2e/gitea.sh $(E2E_IMAGE)

# With -coverpkg every test binary reports every block of the module: the
# awk keeps one line per block and adds up its counts, which leaves the total
# as it is and the file a thirtieth of the size (coverage.out, for Codecov).
test:
	go test $(RACE) -covermode=atomic -coverpkg=./... -coverprofile=coverage.raw -timeout 45m ./...
	awk 'NR == 1 { print; next } \
		{ key = $$1 " " $$2; if (!(key in hits)) order[++n] = key; hits[key] += $$3 } \
		END { for (i = 1; i <= n; i++) print order[i], hits[order[i]] }' coverage.raw >coverage.out
	rm -f coverage.raw
	@go tool cover -func=coverage.out | awk -v min=$(COVERAGE_MIN) ' \
		$$1 == "total:" { sub("%", "", $$3); total = $$3 + 0; found = 1 } \
		END { \
			if (!found) { print "coverage.out has no total"; exit 1 } \
			printf "coverage: %.1f%% of statements (gate %.1f%%)\n", total, min; \
			if (total < min) { print "coverage is below the gate"; exit 1 } \
		}'

build:
	go build -trimpath -o bin/touchmark$(shell go env GOEXE) ./cmd/touchmark

snapshot:
	$(GORELEASER) release --snapshot --clean --skip=sign

docs-serve:
	go run ./scripts/docs
	cp CHANGELOG.md docs/changelog.md
	uv run --no-dev --group docs zensical serve

docs-build:
	go run ./scripts/docs
	cp CHANGELOG.md docs/changelog.md
	uv run --no-dev --group docs zensical build --clean
	uv run --no-dev --group docs python scripts/emit_markdown.py

clean:
	rm -rf bin dist site .cache coverage.out coverage.raw docs/changelog.md
