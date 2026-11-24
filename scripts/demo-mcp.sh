#!/usr/bin/env bash
# MCP 冒烟验收：起真实 server + 官方 filesystem MCP server（stdio），让 agent
# 只用外部工具读一个文件，走完「列工具 → 外部调用审批 → 执行 → 回填」。
#
# 与 internal/mcp/smoke_test.go（-tags mcpsmoke）互补：那边断言适配器，
# 这里验证整条人工链路（审批面板、事件、审计）。
#
# ── 必需环境变量（也可写进 ~/.config/jiaoag/demo.env） ──────────────
#   JIAOAG_DEMO_BASE_URL   OpenAI / Anthropic 兼容端点
#   JIAOAG_DEMO_MODEL      模型 id
#   JIAOAG_DEMO_API_KEY    密钥本身；或改用 JIAOAG_DEMO_API_KEY_FILE 指向文件
#
# ── 可选 ────────────────────────────────────────────────────────────
#   JIAOAG_DEMO_PROVIDER_TYPE  openai（默认）| anthropic
#   JIAOAG_DEMO_MCP_PORT       本地监听端口，默认 18788
#   JIAOAG_DEMO_APPROVE        审批策略，默认 allow_once
#   JIAOAG_DEMO_KEEP=1         成功时也保留临时目录
#   JIAOAG_DEMO_MCP_SERVER     覆盖 stdio server 命令（默认 npx -y
#                              @modelcontextprotocol/server-filesystem）
#
# 依赖：npx / node（第一次会从 npm 拉取 server）。
# 用法：make demo-mcp

set -euo pipefail

ROOT=$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)
cd "$ROOT"

DEMO_ENV="${HOME}/.config/jiaoag/demo.env"
if [[ -f "$DEMO_ENV" ]]; then
  # shellcheck disable=SC1090
  source "$DEMO_ENV"
fi

if [[ -n "${JIAOAG_DEMO_API_KEY_FILE:-}" && -z "${JIAOAG_DEMO_API_KEY:-}" ]]; then
  JIAOAG_DEMO_API_KEY=$(cat "$JIAOAG_DEMO_API_KEY_FILE")
fi
export JIAOAG_DEMO_API_KEY="${JIAOAG_DEMO_API_KEY:-}"

missing=()
[[ -n "${JIAOAG_DEMO_BASE_URL:-}" ]] || missing+=("JIAOAG_DEMO_BASE_URL")
[[ -n "${JIAOAG_DEMO_MODEL:-}" ]] || missing+=("JIAOAG_DEMO_MODEL")
[[ -n "${JIAOAG_DEMO_API_KEY:-}" ]] || missing+=("JIAOAG_DEMO_API_KEY（或 JIAOAG_DEMO_API_KEY_FILE）")
if [[ ${#missing[@]} -gt 0 ]]; then
  echo "缺少环境变量：${missing[*]}" >&2
  echo "见 scripts/demo-mcp.sh 头部说明，或写入 $DEMO_ENV" >&2
  exit 2
fi

if ! command -v npx >/dev/null 2>&1 && [[ -z "${JIAOAG_DEMO_MCP_SERVER:-}" ]]; then
  echo "缺少 npx：MCP 冒烟默认用 npx 拉取官方 filesystem server。" >&2
  echo "安装 Node.js，或用 JIAOAG_DEMO_MCP_SERVER 指定一个现成的 stdio server。" >&2
  exit 2
fi

PROVIDER_TYPE="${JIAOAG_DEMO_PROVIDER_TYPE:-openai}"
PORT="${JIAOAG_DEMO_MCP_PORT:-18788}"
APPROVE="${JIAOAG_DEMO_APPROVE:-allow_once}"

WORK=$(mktemp -d "${TMPDIR:-/tmp}/jiaoag-mcp-demo.XXXXXX")
PROJECT="$WORK/project"
SERVER_PID=""
MARKER="mcp-demo-ok-$(date +%s)"

cleanup() {
  local status=$?
  if [[ -n "$SERVER_PID" ]] && kill -0 "$SERVER_PID" 2>/dev/null; then
    kill "$SERVER_PID" 2>/dev/null || true
    wait "$SERVER_PID" 2>/dev/null || true
  fi
  if [[ $status -ne 0 ]]; then
    echo "MCP 冒烟失败：现场保留在 $WORK" >&2
    if [[ -f "$WORK/server.log" ]]; then
      echo "--- server.log 尾部 ---" >&2
      tail -30 "$WORK/server.log" >&2
    fi
  elif [[ "${JIAOAG_DEMO_KEEP:-0}" == "1" ]]; then
    echo "现场保留在 $WORK"
  else
    rm -rf "$WORK"
  fi
}
trap cleanup EXIT

# ── 1. 临时工作区 ──────────────────────────────────────────────────────
mkdir -p "$PROJECT" "$WORK/data/policy.d" "$WORK/bin"
printf '%s\n' "$MARKER" > "$PROJECT/hello.txt"

# ── 2. 临时配置（内置工具仍可用，但 prompt 要求只走 MCP） ──────────────
if [[ -n "${JIAOAG_DEMO_MCP_SERVER:-}" ]]; then
  # 允许用户给一串「命令 + 参数」，按空白切分；路径含空格请自行包一层脚本。
  # shellcheck disable=SC2206
  MCP_ARGV=(${JIAOAG_DEMO_MCP_SERVER})
  MCP_YAML_ARGS=""
  for arg in "${MCP_ARGV[@]:1}"; do
    MCP_YAML_ARGS+="        - $(printf '%s' "$arg" | sed 's/"/\\"/g')
"
  done
else
  MCP_ARGV=(npx -y @modelcontextprotocol/server-filesystem "$PROJECT")
  MCP_YAML_ARGS="        - \"-y\"
        - \"@modelcontextprotocol/server-filesystem\"
        - \"$PROJECT\"
"
fi

cat > "$WORK/config.yaml" <<EOF
server:
  listen: 127.0.0.1:${PORT}
  token: demo-token

storage:
  db_path: ${WORK}/data/jiaoag.db
  output_dir: ${WORK}/data/outputs

policy:
  dir: ${WORK}/data/policy.d

exec:
  backend: ${JIAOAG_DEMO_EXEC_BACKEND:-direct}
  timeout_s: 60

agent:
  max_rounds: 8

providers:
  - name: demo
    type: ${PROVIDER_TYPE}
    base_url: ${JIAOAG_DEMO_BASE_URL}
    model: ${JIAOAG_DEMO_MODEL}
    api_key: \$JIAOAG_DEMO_API_KEY
    timeout_seconds: 120
    max_retries: 1

mcp:
  servers:
    - name: fs
      command: "${MCP_ARGV[0]}"
      args:
${MCP_YAML_ARGS}      timeout_s: 60
      trust: ask
EOF

echo "构建中…"
go build -o "$WORK/bin/jiaoag" ./cmd/jiaoag

# ── 3. 起真实 server ──────────────────────────────────────────────────
"$WORK/bin/jiaoag" serve -config "$WORK/config.yaml" >"$WORK/server.log" 2>&1 &
SERVER_PID=$!

for _ in $(seq 1 80); do
  if curl -sf "127.0.0.1:${PORT}/healthz" >/dev/null 2>&1; then
    break
  fi
  if ! kill -0 "$SERVER_PID" 2>/dev/null; then
    echo "server 启动失败" >&2
    cat "$WORK/server.log" >&2
    exit 1
  fi
  sleep 0.25
done
curl -sf "127.0.0.1:${PORT}/healthz" >/dev/null || {
  echo "server 在 20 秒内没有就绪（端口 ${PORT} 可能被占用）" >&2
  exit 1
}
echo "server 已就绪：127.0.0.1:${PORT}（provider=${PROVIDER_TYPE} model=${JIAOAG_DEMO_MODEL}）"
echo

# ── 4. 只走 MCP 工具读文件 ─────────────────────────────────────────────
set +e
"$WORK/bin/jiaoag" demo \
  -url "ws://127.0.0.1:${PORT}/ws" \
  -token demo-token \
  -workspace "$PROJECT" \
  -provider demo \
  -approve "$APPROVE" \
  -timeout 3m \
  -prompt "请只使用 mcp__fs__read_text_file 工具读取工作区根目录下的 hello.txt，把文件内容原样复述一遍。不要使用 read_file 工具，也不要修改任何文件。" \
  >"$WORK/demo.log" 2>&1
DEMO_STATUS=$?
set -e
cat "$WORK/demo.log"

# ── 5. 核验：MCP 工具被调用、内容回填、审批与审计落库 ─────────────────
echo
echo "=== 核验（不经过 agent） ==="

if [[ $DEMO_STATUS -ne 0 ]]; then
  echo "demo 退出码 ${DEMO_STATUS}" >&2
  exit 1
fi
if ! grep -q 'mcp__fs__read_text_file' "$WORK/demo.log"; then
  echo "核验失败：模型没有调用 MCP 工具" >&2
  exit 1
fi
echo "✓ 模型调用了 mcp__fs__read_text_file"
if ! grep -q "$MARKER" "$WORK/demo.log"; then
  echo "核验失败：工具结果没有回填到对话" >&2
  exit 1
fi
echo "✓ 文件内容经 MCP 工具回填：$MARKER"
if grep -q '→ read_file' "$WORK/demo.log"; then
  echo "提示：模型同时用了内置 read_file；MCP 链路本身已通过。" >&2
fi

if command -v sqlite3 >/dev/null 2>&1; then
  echo
  echo "=== 审计 ==="
  sqlite3 -header -column "$WORK/data/jiaoag.db" \
    "select action, actor, substr(detail, 1, 120) as detail from audit order by rowid;"
  if ! sqlite3 "$WORK/data/jiaoag.db" "select 1 from audit where action='external.approved' limit 1;" | grep -q 1; then
    echo "核验失败：没有 external.approved 审计" >&2
    exit 1
  fi
  echo "✓ 外部调用已入审计（external.approved）"
else
  echo "（未安装 sqlite3，跳过审计核验）"
fi

echo
echo "✅ MCP 冒烟通过"