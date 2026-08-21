MODULE := github.com/cyber-shuttle/linkspan
BIN    := bin

# Every binary carries the tag it was built from. A release is X.Y.Z and never
# carries a commit; a build ahead of one is X.Y.Z.<commit>, and that commit makes
# every such build a different version, so a newer one always replaces an older
# one and two binaries can never claim the same version. A build with no tag to
# name would report "dev", which sorts above every release wherever it is
# installed and would never be replaced, so an untagged commit is refused here.
VERSION := $(patsubst v%,%,$(shell git describe --tags --exact-match 2>/dev/null))
VALID   := $(shell printf '%s' '$(VERSION)' | grep -Eo '^[0-9]+\.[0-9]+\.[0-9]+(\.[0-9a-f]{7,40})?$$')
COMMIT  := $(shell git rev-parse HEAD 2>/dev/null)

PLATFORMS := linux/amd64 linux/arm64 darwin/amd64 darwin/arm64

.PHONY: all clean

all: $(foreach p,$(PLATFORMS),$(BIN)/linkspan-$(subst /,-,$(p)))

$(BIN)/linkspan-%:
	$(eval GOOS   := $(word 1,$(subst -, ,$*)))
	$(eval GOARCH := $(word 2,$(subst -, ,$*)))
	@mkdir -p $(BIN)
	@[ -n "$(VALID)" ] || { echo "refusing to build: HEAD needs a tag of the form X.Y.Z or X.Y.Z.<commit> (found '$(VERSION)')"; exit 1; }
	@case "$(VERSION)" in *.*.*.*) case "$(COMMIT)" in "$(lastword $(subst ., ,$(VERSION)))"*) ;; \
	  *) echo "refusing to build: tag $(VERSION) does not name this commit $(COMMIT)"; exit 1 ;; esac ;; esac
	GOOS=$(GOOS) GOARCH=$(GOARCH) CGO_ENABLED=0 go build -ldflags "-X main.version=$(patsubst v%,%,$(VERSION))" -o $@ $(MODULE)
	@echo "built $@"

clean:
	rm -rf $(BIN)
