TARGET_DIR = /home/cwhypt/cliproxyapi/plugins/linux/arm64
PLUGIN_VERSION = 0.3.3
PLUGIN_SO = $(TARGET_DIR)/cpa-codex-guard-v$(PLUGIN_VERSION).so
GO ?= go

.PHONY: all build test clean

all: test build

test:
	$(GO) test -v ./...

build:
	mkdir -p $(TARGET_DIR)
	CGO_ENABLED=1 GOOS=linux GOARCH=arm64 $(GO) build -buildvcs=false -tags cshared -buildmode=c-shared -o $(PLUGIN_SO) ./cmd/cpa-codex-guard

clean:
	rm -f $(PLUGIN_SO) $(TARGET_DIR)/cpa-codex-guard-v$(PLUGIN_VERSION).h
