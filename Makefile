MODULE := github.com/cyber-shuttle/linkspan
BIN    := bin

PLATFORMS := linux/amd64 linux/arm64 darwin/amd64 darwin/arm64

.PHONY: all clean

all: $(foreach p,$(PLATFORMS),$(BIN)/linkspan-$(subst /,-,$(p)))

$(BIN)/linkspan-%:
	$(eval GOOS   := $(word 1,$(subst -, ,$*)))
	$(eval GOARCH := $(word 2,$(subst -, ,$*)))
	@mkdir -p $(BIN)
	GOOS=$(GOOS) GOARCH=$(GOARCH) CGO_ENABLED=0 go build -o $@ $(MODULE)
	@echo "built $@"

clean:
	rm -rf $(BIN)
