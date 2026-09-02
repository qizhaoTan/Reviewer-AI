BIN     := cr
PKG     := ./cmd/reviewer-ai
VERSION := $(shell git describe --tags --always --dirty 2>/dev/null || echo dev)
LDFLAGS := -s -w -X main.version=$(VERSION)

# 分发目标从本地未跟踪的 deploy.mk 读取，仓库里不保留任何主机信息。
# 用法：复制 deploy.mk.example 为 deploy.mk，填入自己的 DIST_TARGETS / UPLOAD_TARGETS。
-include deploy.mk

all:    regenerate

regenerate:
	go build -ldflags "$(LDFLAGS)" -o $(BIN) $(PKG)

test:
	go test ./... 2>&1

win:
	CGO_ENABLED=0 GOOS=windows GOARCH=amd64 go build -ldflags "$(LDFLAGS)" -o bin/$(BIN).exe $(PKG)

linux:
	CGO_ENABLED=0 GOOS=linux GOARCH=amd64 go build -ldflags "$(LDFLAGS)" -o bin/$(BIN) $(PKG)

mac:
	CGO_ENABLED=0 GOOS=darwin GOARCH=amd64 go build -ldflags "$(LDFLAGS)" -o bin/$(BIN)-mac $(PKG)

mac-arm:
	CGO_ENABLED=0 GOOS=darwin GOARCH=arm64 go build -ldflags "$(LDFLAGS)" -o bin/$(BIN)-mac-arm $(PKG)

cross: win linux mac mac-arm

# DIST_TARGETS / UPLOAD_TARGETS 形如 "user@host:/目标路径"，多个用空格分隔，
# 在 deploy.mk 里定义；需要非标端口等 scp 额外参数时用 SCP_FLAGS（对所有目标统一生效）。
# 未定义时这两个 target 直接提示而不做任何事。
dist: cross
	@if [ -z "$(DIST_TARGETS)" ]; then \
		echo "未配置 DIST_TARGETS，请参考 deploy.mk.example 创建 deploy.mk"; exit 1; \
	fi
	@for t in $(DIST_TARGETS); do \
		echo "scp -> $$t"; \
		scp $(SCP_FLAGS) bin/$(BIN) bin/$(BIN).exe "$$t" || exit 1; \
	done

upload: linux
	@if [ -z "$(UPLOAD_TARGETS)" ]; then \
		echo "未配置 UPLOAD_TARGETS，请参考 deploy.mk.example 创建 deploy.mk"; exit 1; \
	fi
	@for t in $(UPLOAD_TARGETS); do \
		echo "scp -> $$t"; \
		scp $(SCP_FLAGS) bin/$(BIN) "$$t" || exit 1; \
	done

clean:
	rm -rf bin $(BIN)

.PHONY: all regenerate test win linux mac mac-arm cross dist upload clean
