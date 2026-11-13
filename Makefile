GO ?= go
STATICCHECK_VERSION ?= v0.8.2
STATICCHECK = $(GO) run honnef.co/go/tools/cmd/staticcheck@$(STATICCHECK_VERSION)

.PHONY: check fmt vet staticcheck tidy-check test build

# 本地与 CI 共用的静态检查入口。
check: fmt vet staticcheck tidy-check test

fmt:
	@out="$$(gofmt -l .)"; \
	if [ -n "$$out" ]; then \
		echo "以下文件未格式化："; \
		echo "$$out"; \
		exit 1; \
	fi

vet:
	$(GO) vet ./...

staticcheck:
	$(STATICCHECK) ./...

tidy-check:
	$(GO) mod tidy -diff

test:
	$(GO) test ./...

build:
	$(GO) build -o bin/jiaoag-server ./cmd/jiaoag-server
	$(GO) build -o bin/jiaoag-tui ./cmd/jiaoag-tui
