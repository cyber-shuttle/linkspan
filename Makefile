MODULE := github.com/cyber-shuttle/linkspan
BIN    := bin

# Pinned so a local run and a CI run report the same thing.
GOLANGCI := v2.13.2
GOVULN   := v1.8.0

# A release tag is X.Y.Z; a build ahead of one is X.Y.Z.<commit>, a distinct newer version.
# Untagged is refused: it would report "dev", which fails the version check cs-bridge runs on every launch.
VERSION := $(patsubst v%,%,$(shell git describe --tags --exact-match 2>/dev/null))
VALID   := $(shell printf '%s' '$(VERSION)' | grep -Eo '^[0-9]+\.[0-9]+\.[0-9]+(\.[0-9a-f]{7,40})?$$')

PLATFORMS := linux/amd64 linux/arm64 darwin/amd64 darwin/arm64

.PHONY: all check tools fmt vet lint vuln test clean FORCE

all: $(foreach p,$(PLATFORMS),$(BIN)/linkspan-$(subst /,-,$(p)))

# FORCE runs the recipe every time; make would otherwise ship an existing binary with a stale tag.
$(BIN)/linkspan-%: FORCE
	$(eval GOOS   := $(word 1,$(subst -, ,$*)))
	$(eval GOARCH := $(word 2,$(subst -, ,$*)))
	@mkdir -p $(BIN)
	@[ -n "$(VALID)" ] || { echo "refusing to build: HEAD needs a tag X.Y.Z or X.Y.Z.<commit> (found '$(VERSION)')"; exit 1; }
	GOOS=$(GOOS) GOARCH=$(GOARCH) CGO_ENABLED=0 go build -ldflags "-X main.version=$(VERSION)" -o $@ $(MODULE)

# What CI runs, in the order that fails fastest.
check: fmt vet lint vuln test

tools:
	go install github.com/golangci/golangci-lint/v2/cmd/golangci-lint@$(GOLANGCI)
	go install golang.org/x/vuln/cmd/govulncheck@$(GOVULN)

fmt:
	golangci-lint fmt --diff

vet:
	go vet ./...

lint:
	golangci-lint run ./...

vuln:
	govulncheck ./...

test:
	go test -race ./...

clean:
	rm -rf $(BIN)
