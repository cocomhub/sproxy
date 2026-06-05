VERSION ?= $(shell git describe --tags --always --dirty)
BUILD_DIR ?= build
BUILD_AT ?= $(shell date +"%Y-%m-%dT%H:%M:%SZ")

GOOS ?= $(shell go env GOOS)
GOARCH ?= $(shell go env GOARCH)
GO ?= GOOS=$(GOOS) GOARCH=$(GOARCH) go
GOFLAGS ?= -v
GO_LDFLAGS ?= -w -s
GO_LDFLAGS += -X main.Version=$(VERSION) -X main.BuildAt=$(BUILD_AT)

PROJECT_NAME ?= sproxy
BIN_DIR ?= $(BUILD_DIR)/bin
BIN_NAME ?= $(BIN_DIR)/$(PROJECT_NAME)-$(GOOS)-$(GOARCH)
CONFIG_FILE ?= $(BUILD_DIR)/config.yaml

GOFMT=gofmt

# OS binary suffix
ifeq ($(OS),Windows_NT)
EXE := .exe
else
EXE :=
endif

# Static list of commands (cross-platform: no shell for-loop needed).
CMD_NAMES := sproxy sclient
# all .go files using go list (cross-platform).
ALL_SRC = $(shell go list -f '{{range .GoFiles}}{{$$.Dir}}/{{.}} {{end}}' ./...)

.PHONY: build build-%

build: fmt
	@mkdir -p $(BIN_DIR)
	@$(foreach name,$(CMD_NAMES),echo "Building $(name)"; GOOS=$(GOOS) GOARCH=$(GOARCH) go build $(GOFLAGS) -ldflags "$(GO_LDFLAGS)" -o $(BIN_DIR)/$(name)$(EXE) ./cmd/$(name);)

build-%: fmt
	@mkdir -p $(BIN_DIR)
	@echo "Building $*"
	@GOOS=$(GOOS) GOARCH=$(GOARCH) go build $(GOFLAGS) -ldflags "$(GO_LDFLAGS)" -o $(BIN_DIR)/$*$(EXE) ./cmd/$*

# 格式化目标
.PHONY: fmt
fmt: addlicense fix
	@echo Running gofmt on ALL_SRC ...
	@$(GOFMT) -e -s -l -w $(ALL_SRC)
	@echo Running gofumpt on ALL_SRC ...
# 	@$(GOFUMPT) -e -l -w $(ALL_SRC)

# 添加许可证
.PHONY: addlicense
addlicense:
	addlicense -c "The Cocomhub Authors. All rights reserved." -s=only .

# 修复目标
.PHONY: fix
fix:
	@echo Running go fix ./...
	@$(GO) fix ./...

.PHONY: clean

clean:
	rm -rf $(BIN_DIR)

.PHONY: vet

vet:
	@echo Running go vet ./...
	@$(GO) vet ./...

.PHONY: test

test: vet
	@echo Running go test -race ./...
	@$(GO) test -race ./...

.PHONY: cover

cover: test
	@mkdir -p $(BUILD_DIR)/coverage
	$(GO) test -race -coverprofile=$(BUILD_DIR)/coverage/cover.out ./...
	$(GO) tool cover -html=$(BUILD_DIR)/coverage/cover.out -o $(BUILD_DIR)/coverage/cover.html
	@$(GO) tool cover -func=$(BUILD_DIR)/coverage/cover.out
	@echo "Coverage report: file://$(BUILD_DIR)/coverage/cover.html"

.PHONY: run

run: build
	$(BIN_NAME) --config $(CONFIG_FILE)

.PHONY: show-version

show-version:
	$(BIN_NAME) --version