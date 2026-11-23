GO ?= go
STATICCHECK_VERSION ?= v0.8.2
STATICCHECK = $(GO) run honnef.co/go/tools/cmd/staticcheck@$(STATICCHECK_VERSION)

.PHONY: check fmt vet staticcheck tidy-check test build demo demo-mcp

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

# 真实 provider 冒烟：起真实 server，在临时项目上跑一轮读→改→测。
# 需要 JIAOAG_DEMO_* 环境变量（或 ~/.config/jiaoag/demo.env），见 scripts/demo.sh。
demo:
	@scripts/demo.sh

# MCP 冒烟：官方 filesystem server（stdio）+ 真实 provider，只走外部工具读文件。
# 需要 npx/node 与 JIAOAG_DEMO_* 环境变量，见 scripts/demo-mcp.sh。
demo-mcp:
	@scripts/demo-mcp.sh
