APP_NAME := pfgots
DIST_DIR := dist
TMP_DIR := $(CURDIR)/.tmp

TS_VERSION ?= $(shell cat vendor/tailscale.com/VERSION.txt 2>/dev/null || echo "1.104.4")
VERSION_FLAGS := -X tailscale.com/version.longStamp=$(TS_VERSION) -X tailscale.com/version.shortStamp=$(TS_VERSION)

GO_ENV := GOTOOLCHAIN=local TMPDIR=$(TMP_DIR) GOTMPDIR=$(TMP_DIR) GOCACHE=$(TMP_DIR)/go-build GOPATH=$(TMP_DIR)/gopath
GO_FLAGS := -mod=vendor -trimpath -ldflags="-s -w $(VERSION_FLAGS)"
SRC_ENTRY := ./src

.PHONY: all build clean dist dist-darwin-amd64 dist-darwin-arm64 dist-linux-amd64 dist-linux-arm64 dist-windows-amd64 test

all: dist

build:
	@mkdir -p $(DIST_DIR) $(TMP_DIR)
	$(GO_ENV) go build $(GO_FLAGS) -o $(DIST_DIR)/$(APP_NAME) $(SRC_ENTRY)

test:
	@mkdir -p $(TMP_DIR)
	$(GO_ENV) go test -mod=vendor -v ./src/...

clean:
	rm -rf $(DIST_DIR) $(TMP_DIR)

dist:
	rm -rf $(DIST_DIR)
	@mkdir -p $(DIST_DIR) $(TMP_DIR)
	$(MAKE) dist-darwin-arm64
	$(MAKE) dist-darwin-amd64
	$(MAKE) dist-linux-amd64
	$(MAKE) dist-linux-arm64
	$(MAKE) dist-windows-amd64
	@cd $(DIST_DIR) && shasum -a 256 * > SHA256SUMS

dist-darwin-arm64:
	@mkdir -p $(DIST_DIR) $(TMP_DIR)
	rm -f $(DIST_DIR)/$(APP_NAME)-darwin-arm64
	GOOS=darwin GOARCH=arm64 $(GO_ENV) go build $(GO_FLAGS) -o $(DIST_DIR)/$(APP_NAME)-darwin-arm64 $(SRC_ENTRY)

dist-darwin-amd64:
	@mkdir -p $(DIST_DIR) $(TMP_DIR)
	rm -f $(DIST_DIR)/$(APP_NAME)-darwin-amd64
	GOOS=darwin GOARCH=amd64 $(GO_ENV) go build $(GO_FLAGS) -o $(DIST_DIR)/$(APP_NAME)-darwin-amd64 $(SRC_ENTRY)

dist-linux-amd64:
	@mkdir -p $(DIST_DIR) $(TMP_DIR)
	rm -f $(DIST_DIR)/$(APP_NAME)-linux-amd64
	GOOS=linux GOARCH=amd64 $(GO_ENV) go build $(GO_FLAGS) -o $(DIST_DIR)/$(APP_NAME)-linux-amd64 $(SRC_ENTRY)

dist-linux-arm64:
	@mkdir -p $(DIST_DIR) $(TMP_DIR)
	rm -f $(DIST_DIR)/$(APP_NAME)-linux-arm64
	GOOS=linux GOARCH=arm64 $(GO_ENV) go build $(GO_FLAGS) -o $(DIST_DIR)/$(APP_NAME)-linux-arm64 $(SRC_ENTRY)

dist-windows-amd64:
	@mkdir -p $(DIST_DIR) $(TMP_DIR)
	rm -f $(DIST_DIR)/$(APP_NAME)-windows-amd64.exe
	GOOS=windows GOARCH=amd64 $(GO_ENV) go build $(GO_FLAGS) -o $(DIST_DIR)/$(APP_NAME)-windows-amd64.exe $(SRC_ENTRY)
