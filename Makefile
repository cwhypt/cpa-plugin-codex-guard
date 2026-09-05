TARGET_DIR = /home/cwhypt/cliproxyapi/plugins/linux/arm64
PLUGIN_SO = $(TARGET_DIR)/cpa-codex-guard-v0.1.0.so

.PHONY: all build test clean

all: test build

test:
	export PATH=/home/cwhypt/.local/go/bin:$$PATH && go test -v ./...

build:
	mkdir -p $(TARGET_DIR)
	export PATH=/home/cwhypt/.local/go/bin:$$PATH && CGO_ENABLED=1 GOOS=linux GOARCH=arm64 go build -buildvcs=false -tags cshared -buildmode=c-shared -o $(PLUGIN_SO) ./cmd/cpa-codex-guard

clean:
	rm -f $(PLUGIN_SO) $(TARGET_DIR)/cpa-codex-guard-v0.1.0.h
