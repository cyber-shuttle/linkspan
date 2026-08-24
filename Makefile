MODULE := github.com/cyber-shuttle/linkspan
BIN    := bin

# A release tag is X.Y.Z; a build ahead of one is X.Y.Z.<commit>, so it is always a
# distinct, newer version. Untagged is refused: it reports "dev" and outranks every release.
VERSION := $(patsubst v%,%,$(shell git describe --tags --exact-match 2>/dev/null))
VALID   := $(shell printf '%s' '$(VERSION)' | grep -Eo '^[0-9]+\.[0-9]+\.[0-9]+(\.[0-9a-f]{7,40})?$$')

PLATFORMS := linux/amd64 linux/arm64 darwin/amd64 darwin/arm64

.PHONY: all clean FORCE

all: $(foreach p,$(PLATFORMS),$(BIN)/linkspan-$(subst /,-,$(p)))

# FORCE, so the recipe runs every time. With no prerequisites make treats an
# existing bin/linkspan-* as up to date, which shipped a binary built at an
# older tag and skipped the version gate below with it. go build's own cache
# makes an unchanged rebuild cheap.
$(BIN)/linkspan-%: FORCE
	$(eval GOOS   := $(word 1,$(subst -, ,$*)))
	$(eval GOARCH := $(word 2,$(subst -, ,$*)))
	@mkdir -p $(BIN)
	@[ -n "$(VALID)" ] || { echo "refusing to build: HEAD needs a tag X.Y.Z or X.Y.Z.<commit> (found '$(VERSION)')"; exit 1; }
	GOOS=$(GOOS) GOARCH=$(GOARCH) CGO_ENABLED=0 go build -ldflags "-X main.version=$(VERSION)" -o $@ $(MODULE)
	@echo "built $@"

clean:
	rm -rf $(BIN)
