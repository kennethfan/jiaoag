#!/usr/bin/env bash
# 真实 provider 冒烟验收：起真实 server，在临时工作区上跑三轮
#   1) 读代码 → 定位 bug → 出 diff 审批 → 落盘 → 跑测试
#   2) 插件工具（plugin-example 的 echo）
#   3) 附加根读写（-root lib=…）
#
# 与 internal/server 的 realprovider 测试互补：那边断言状态机，这里给人看效果。
#
# ── 必需环境变量（也可写进 ~/.config/jiaoag/demo.env） ──────────────
#   JIAOAG_DEMO_BASE_URL   OpenAI / Anthropic 兼容端点
#                          例：https://apihub.agnes-ai.com/v1
#   JIAOAG_DEMO_MODEL      模型 id，例：agnes-3.0-flash
#   JIAOAG_DEMO_API_KEY    密钥本身；或改用 JIAOAG_DEMO_API_KEY_FILE 指向文件
#
# ── 可选 ────────────────────────────────────────────────────────────
#   JIAOAG_DEMO_PROVIDER_TYPE  openai（默认）| anthropic
#   JIAOAG_DEMO_PORT           本地监听端口，默认 18787
#   JIAOAG_DEMO_APPROVE        审批策略，默认 allow_once
#   JIAOAG_DEMO_EXEC_BACKEND   direct（默认）| sandbox
#   JIAOAG_DEMO_FULL=0         只跑第 1 轮（省时间，跳过插件与附加根）
#   JIAOAG_DEMO_KEEP=1         成功时也保留临时目录
#
# 用法：make demo   或   JIAOAG_DEMO_BASE_URL=... ... scripts/demo.sh

set -euo pipefail

ROOT=$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)
cd "$ROOT"

# 个人默认值放在仓库外，避免把密钥或供应商细节写进版本库。
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
  echo "见 scripts/demo.sh 头部说明，或写入 $DEMO_ENV" >&2
  exit 2
fi

PROVIDER_TYPE="${JIAOAG_DEMO_PROVIDER_TYPE:-openai}"
PORT="${JIAOAG_DEMO_PORT:-18787}"
APPROVE="${JIAOAG_DEMO_APPROVE:-allow_once}"

WORK=$(mktemp -d "${TMPDIR:-/tmp}/jiaoag-demo.XXXXXX")
PROJECT="$WORK/project"
SERVER_PID=""

cleanup() {
  local status=$?
  if [[ -n "$SERVER_PID" ]] && kill -0 "$SERVER_PID" 2>/dev/null; then
    kill "$SERVER_PID" 2>/dev/null || true
    wait "$SERVER_PID" 2>/dev/null || true
  fi
  if [[ $status -ne 0 ]]; then
    echo "冒烟失败：现场保留在 $WORK" >&2
    if [[ -f "$WORK/server.log" ]]; then
      echo "--- server.log 尾部 ---" >&2
      tail -20 "$WORK/server.log" >&2
    fi
  elif [[ "${JIAOAG_DEMO_KEEP:-0}" == "1" ]]; then
    echo "现场保留在 $WORK"
  else
    rm -rf "$WORK"
  fi
}
trap cleanup EXIT

# ── 1. 临时项目：Sum 少加最后一个元素，初始测试是红的 ─────────────────
mkdir -p "$PROJECT/stats" "$WORK/lib" "$WORK/data/policy.d" "$WORK/bin"
printf '附加根内容：LIB-ROOT-OK\n' > "$WORK/lib/lib-notes.txt"
cat > "$PROJECT/go.mod" <<'EOF'
module demo

go 1.24
EOF
cat > "$PROJECT/stats/stats.go" <<'EOF'
package stats

// Sum 返回所有元素之和。
func Sum(nums []int) int {
	total := 0
	for i := 0; i < len(nums)-1; i++ {
		total += nums[i]
	}
	return total
}

// Mean 返回算术平均值；空切片返回 0。
func Mean(nums []int) float64 {
	if len(nums) == 0 {
		return 0
	}
	return float64(Sum(nums)) / float64(len(nums))
}
EOF
cat > "$PROJECT/stats/stats_test.go" <<'EOF'
package stats

import "testing"

func TestSum(t *testing.T) {
	cases := []struct {
		in   []int
		want int
	}{
		{[]int{1, 2, 3}, 6},
		{[]int{5}, 5},
		{[]int{-1, 1}, 0},
		{nil, 0},
	}
	for _, c := range cases {
		if got := Sum(c.in); got != c.want {
			t.Errorf("Sum(%v) = %d, want %d", c.in, got, c.want)
		}
	}
}

func TestMean(t *testing.T) {
	if got := Mean([]int{1, 2, 3}); got != 2 {
		t.Errorf("Mean([1 2 3]) = %v, want 2", got)
	}
}
EOF
cat > "$PROJECT/README.md" <<'EOF'
# demo

冒烟用的临时项目。`stats.Sum` 目前少加最后一个元素。
EOF

# ── 2. 临时配置 + 构建 ─────────────────────────────────────────────────
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
  max_rounds: 12

plugins:
  servers:
    - name: notes
      command: ${WORK}/bin/jiaoag
      args: ["plugin-example"]
      timeout_s: 30

providers:
  - name: demo
    type: ${PROVIDER_TYPE}
    base_url: ${JIAOAG_DEMO_BASE_URL}
    model: ${JIAOAG_DEMO_MODEL}
    api_key: \$JIAOAG_DEMO_API_KEY
    timeout_seconds: 120
    max_retries: 1
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

# ── 4. 跑真实任务 ─────────────────────────────────────────────────────
# 每轮单独建会话（-prompt 之外的参数由 demo 自己解析），输出同时落盘供核验。
demo_run() {
  local name=$1 prompt=$2
  shift 2
  "$WORK/bin/jiaoag" demo \
    -url "ws://127.0.0.1:${PORT}/ws" \
    -token demo-token \
    -workspace "$PROJECT" \
    -provider demo \
    -approve "$APPROVE" \
    -timeout 5m \
    "$@" \
    -prompt "$prompt" 2>&1 | tee "$WORK/${name}.log"
}

echo "── 第 1 轮：读 → 改 → 测 ──"
demo_run round1 "这个项目里 stats.Sum 有 bug：它漏加了最后一个元素。请先读 stats/stats.go 和 stats/stats_test.go 定位问题，修好它，然后运行 go test ./... 确认全部通过。只改必要的地方。"

# ── 5. 独立核验：磁盘内容 + 测试 ──────────────────────────────────────
echo
echo "=== 核验第 1 轮（不经过 agent） ==="
if ! grep -q 'len(nums); i++' "$PROJECT/stats/stats.go"; then
  echo "核验失败：stats.go 里没有预期的修复" >&2
  exit 1
fi
echo "✓ 磁盘已是修复后的循环上界"
(cd "$PROJECT" && go test ./...)
echo "✓ 直接重跑 go test ./... 通过"

if [[ "${JIAOAG_DEMO_FULL:-1}" == "0" ]]; then
  echo "（JIAOAG_DEMO_FULL=0，跳过插件与附加根两轮）"
else
  # ── 6. 插件工具：只声明意图，副作用由宿主中介（ADR-0034） ─────────────
  echo
  echo "── 第 2 轮：插件工具 ──"
  demo_run round2 "只做一件事：调用插件工具 plugin__notes__echo，参数 text 为 PLUGIN-OK，然后把它的返回内容原样告诉我。不要读写任何文件。"

  echo
  echo "=== 核验第 2 轮 ==="
  grep -q 'plugin__notes__echo.*succeeded' "$WORK/round2.log" || {
    echo "核验失败：没有看到 plugin__notes__echo 成功返回" >&2
    exit 1
  }
  grep -q 'PLUGIN-OK' "$WORK/round2.log" || {
    echo "核验失败：插件返回值里没有 PLUGIN-OK" >&2
    exit 1
  }
  echo "✓ 插件工具被调用并回填结果"

  # ── 7. 附加根：读写都用 alias:path，写入走同一套审批/CAS（ADR-0037） ──
  echo
  echo "── 第 3 轮：附加根读写 ──"
  demo_run round3 "工作区挂了一个叫 lib 的附加根。只做两件事：1) 用 read_file 读 lib:lib-notes.txt；2) 用 write_file 把它的内容原样写到 lib:lib-out.txt。不要碰主工作区里的文件。" \
    -root "lib=${WORK}/lib"

  echo
  echo "=== 核验第 3 轮 ==="
  if ! grep -q 'LIB-ROOT-OK' "$WORK/lib/lib-out.txt" 2>/dev/null; then
    echo "核验失败：附加根里没有写出 lib-out.txt" >&2
    exit 1
  fi
  if [[ -e "$PROJECT/lib-out.txt" ]]; then
    echo "核验失败：文件写到了主工作区而不是附加根" >&2
    exit 1
  fi
  echo "✓ 附加根读得到、写得进，主工作区未被污染"
fi

if command -v sqlite3 >/dev/null 2>&1; then
  echo
  echo "=== 审计 ==="
  sqlite3 -header -column "$WORK/data/jiaoag.db" \
    "select action, actor, substr(detail, 1, 100) as detail from audit order by rowid;"
fi

echo
echo "✅ 冒烟通过"