MODULE := github.com/cyber-shuttle/linkspan
BIN    := bin

# Every binary carries the tag it was built from, and a tag is X.Y.Z or
# X.Y.Z.pre. A build with no tag to name would report "dev", which sorts above
# every release wherever it is installed and would never be replaced, so an
# untagged or oddly tagged commit is refused here instead.
VERSION := $(patsubst v%,%,$(shell git describe --tags --exact-match 2>/dev/null))
VALID   := $(shell printf '%s' '$(patsubst v%,%,$(shell git describe --tags --exact-match 2>/dev/null))' | grep -Eo '^[0-9]+\.[0-9]+\.[0-9]+(\.pre)?$$')

PLATFORMS := linux/amd64 linux/arm64 darwin/amd64 darwin/arm64

.PHONY: all clean

all: $(foreach p,$(PLATFORMS),$(BIN)/linkspan-$(subst /,-,$(p)))

$(BIN)/linkspan-%:
	$(eval GOOS   := $(word 1,$(subst -, ,$*)))
	$(eval GOARCH := $(word 2,$(subst -, ,$*)))
	@mkdir -p $(BIN)
	@[ -n "$(VALID)" ] || { echo "refusing to build: HEAD needs a tag of the form X.Y.Z or X.Y.Z.pre (found '$(VERSION)')"; exit 1; }
	GOOS=$(GOOS) GOARCH=$(GOARCH) CGO_ENABLED=0 go build -ldflags "-X main.version=$(patsubst v%,%,$(VERSION))" -o $@ $(MODULE)
	@echo "built $@"

clean:
	rm -rf $(BIN)
