.PHONY: all build build-dev linux darwin windows test lint clean release tag

GO ?= go
VERSION ?= $(shell tr -d ' \n\r' < VERSION 2>/dev/null || echo "dev")
LDFLAGS := -X main.version=$(VERSION)

# 独立的一份测试插件：产物名 codearts-dev.* 决定 CPA 的 plugin id，
# 与生产插件同时装在一台机器上也不冲突（provider 名仍是 codearts）。
DEV_VERSION ?= $(VERSION)-dev
DEV_LDFLAGS := -X main.version=$(DEV_VERSION)

# build-darwin 的目标架构（可覆盖：DARWIN_ARCH=amd64）。
DARWIN_ARCH ?= $(shell $(GO) env GOARCH)

# 默认目标：给当前平台构建生产插件。
build:
	CGO_ENABLED=1 $(GO) build -trimpath -buildmode=c-shared -ldflags "$(LDFLAGS)" -o codearts$(if $(filter Windows_NT,$(OS)),.dll,.so) ./internal

build-dev:
	CGO_ENABLED=1 $(GO) build -trimpath -buildmode=c-shared -ldflags "$(DEV_LDFLAGS)" -o codearts-dev$(if $(filter Windows_NT,$(OS)),.dll,.so) ./internal

linux:
	@test "$(shell $(GO) env GOOS)" = linux || (echo 'make linux 需要在 linux 上执行'; exit 1)
	CGO_ENABLED=1 $(GO) build -trimpath -buildmode=c-shared -ldflags "$(LDFLAGS)" -o codearts.so ./internal

darwin:
	@test "$(shell $(GO) env GOOS)" = darwin || (echo 'make darwin 需要在 macOS 上执行'; exit 1)
	MACOSX_DEPLOYMENT_TARGET=11.0 CGO_ENABLED=1 $(GO) build -trimpath -buildmode=c-shared -ldflags "$(LDFLAGS)" -o codearts.dylib ./internal

windows:
	@test "$(shell $(GO) env GOOS)" = windows || (echo 'make windows 需要在 windows 上执行'; exit 1)
	CGO_ENABLED=1 $(GO) build -trimpath -buildmode=c-shared -ldflags "$(LDFLAGS)" -o codearts.dll ./internal

test:
	$(GO) test -count=1 ./...

# 格式 + 静态检查；装了 staticcheck 就一起跑。
lint:
	@test -z "$$($(GO) fmt -l .)" || ($(GO) fmt -l . && exit 1)
	$(GO) vet ./...
	@if command -v staticcheck >/dev/null 2>&1; then staticcheck ./...; fi

# 发布自检：校验 registry.json + 测试 + 构建插件
check: lint test build
	@python3 scripts/validate-registry.py registry.json || python scripts/validate-registry.py registry.json

clean:
	rm -f codearts.so codearts.dll codearts.dylib codearts.h
	rm -f codearts-dev.so codearts-dev.dll codearts-dev.h
	rm -rf dist/ out/ coverage.out coverage.html

# 跨平台打包（需要对应平台的 C 工具链）。
release: clean
	@mkdir -p dist
	@for target in linux/amd64 linux/arm64; do \
	  os=$${target%/*}; arch=$${target#*/}; \
	  out=dist/$(VERSION)/$${os}_$${arch}; \
	  mkdir -p $$out; \
	  GOOS=$$os GOARCH=$$arch CGO_ENABLED=1 $(GO) build -trimpath -buildmode=c-shared -ldflags "$(LDFLAGS)" -o $$out/codearts.so ./internal || exit 1; \
	  rm -f $$out/*.h; \
	  $(GO) run ./.github/scripts/package-release.go -library $$out/codearts.so \
	    -archive codearts_$(VERSION)_$${os}_$${arch}.zip \
	    -checksum codearts_$(VERSION)_$${os}_$${arch}.zip.sha256; \
	done

# 打发布标签，CI 会据此产出 GitHub Release（目录元数据只认 v<version>）。
tag:
	git tag -a v$(VERSION) -m "CodeArts CPA v$(VERSION)"
	git push origin v$(VERSION)
