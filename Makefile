.PHONY: all build linux windows test vet check clean

GO ?= go

all: build

# 产物名随宿主平台走：Windows 是 .dll，其余是 .so
build:
	CGO_ENABLED=1 $(GO) build -buildmode=c-shared -o codearts$(if $(filter Windows_NT,$(OS)),.dll,.so) ./plugin/codearts

linux:
	@test "$(shell $(GO) env GOOS)" = linux || (echo 'make linux 需要在 linux 上执行'; exit 1)
	CGO_ENABLED=1 $(GO) build -buildmode=c-shared -o codearts.so ./plugin/codearts

windows:
	@test "$(shell $(GO) env GOOS)" = windows || (echo 'make windows 需要在 windows 上执行'; exit 1)
	CGO_ENABLED=1 $(GO) build -buildmode=c-shared -o codearts.dll ./plugin/codearts

test:
	$(GO) test -count=1 ./...

vet:
	$(GO) vet ./...

check: vet test build

clean:
	rm -f codearts.so codearts.dll codearts.h
