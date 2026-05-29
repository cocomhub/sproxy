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

# Discover main packages under ./cmd/*
CMD_IMPORTS := $(shell go list -f '{{if eq .Name "main"}}{{.ImportPath}}{{end}}' ./cmd/* 2>/dev/null | sed '/^$$/d')
CMD_NAMES := $(notdir $(CMD_IMPORTS))
# all .go files that are not auto-generated and should be auto-formatted and linted.
ALL_SRC = $(shell find . -name '*.go' \
				   -not -name 'doc.go' \
				   -not -name '_*' \
				   -not -name '.*' \
				   -not -name 'mocks*' \
				   -not -name 'model.pb.go' \
				   -not -name 'model_test.pb.go' \
				   -not -name 'storage_test.pb.go' \
				   -not -path './examples/*' \
				   -not -path './vendor/*' \
				   -not -path '*/mocks/*' \
				   -not -path '*/*-gen/*' \
				   -type f | \
				sort)

.PHONY: build build-%

build: fmt
	@mkdir -p $(BIN_DIR)
	@set -e; \
	if [ -z "$(CMD_NAMES)" ]; then \
	  echo "No commands found under ./cmd"; \
	else \
	  for name in $(CMD_NAMES); do \
	    echo "Building $$name"; \
	    GOOS=$(GOOS) GOARCH=$(GOARCH) go build $(GOFLAGS) -ldflags "$(GO_LDFLAGS)" -o $(BIN_DIR)/$$name$(EXE) ./cmd/$$name ; \
	  done; \
	fi

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

.PHONY: test

test:
	@echo Running go test -race ./...
	@$(GO) test -race ./...

.PHONY: run

run: build
	$(BIN_NAME) --config $(CONFIG_FILE)

.PHONY: show-version

show-version:
	$(BIN_NAME) --version
