# 权限扩展只提供数据，不提供判定

shell 解析漏洞、CVE、新型注入手法这类情报来得比发版快，靠内置规则和重启跟不上。但权限策略是唯一安全边界（ADR-0001），把它开放给外部代码等于把门钥匙交出去：一个能返回「自动放行」的插件，可以让任意命令绕过审批，而且审计里看不出异常，因为判定本身就是合法的。

因此拆成三层，只有中间那层可变：

| 层 | 归属 | 可变性 |
|---|---|---|
| 权限引擎（短路顺序、argv 匹配、走审批与 CAS 的交互） | 我们的代码 | 随版本 |
| 规则集（危险模式、CVE 相关约束、环境条件） | 数据 | **可热更新** |
| 规则来源（内嵌默认、本地文件、远程签名 feed） | `perm.Loader` | 可扩展 |

## 外部规则只能收紧

这条用类型表达，不靠文档约定：

```go
// perm：第三方规则唯一能拿到的接口
type TightenRule interface {
	// 只允许返回 NeedsApproval 或 Denied；返回 nil 表示不表态
	Evaluate(ctx context.Context, call domain.PreparedCall, env Env) *Tighten
}

type Tighten struct {
	Verdict domain.Verdict // 仅 NeedsApproval | Denied
	Reason  string
	RuleID  string
	Source  string // feed:<name>@<version>
}
```

引擎里没有把规则输出降级为 `Auto` 的路径。放宽只能靠本地人工动作：改 `~/.config/jiaoag/policy.d/*.yaml` 并重启，或执行显式的 `jiaoag policy trust`。远程 feed 若能放宽，整条安全链就变成后门。

## Feed 的工程要求

- **签名**：ed25519，公钥固定在配置里；验签失败整批丢弃。
- **防回滚**：`version` 单调递增并带 `min_version`；旧 feed 重放属于降级攻击。
- **TTL**：过期不撤销已生效规则，只在 TUI 常驻提示 `security feed stale` 并写审计。过期即回退会让用户误以为没在被管。
- **失败模式**：拉取失败沿用最后一份 good snapshot；内嵌默认规则永远加载，任何情况下不会出现「零规则」。
- **规则不可执行**：只做声明式匹配（argv 首 token、子串、正则、路径前缀），不携带脚本。Go 的 `regexp` 是 RE2，无回溯爆炸，但仍限制单条 ≤256 B、总量 ≤2000 条，防止规则膨胀拖慢每次工具调用。
- **环境感知**：条件由引擎求值，插件只提供规则，不接触环境探测代码。

```yaml
- id: bash-parser-cve-2025-xxxx
  when: { os: darwin, shell: bash, shell_version: "<5.2" }
  match: { tool: run_command, any_arg_contains: ["$(", "`"] }
  verdict: NeedsApproval
  reason: "当前 bash 版本存在解析绕过风险"
  source: feed:secops@2026.11.1
```

## 热更新与在途任务

规则集是不可变快照 `Ruleset{Version, Hash, Rules}`，`Loader` 产出新快照后原子替换指针；`perm.Decision` 带 `PolicyVersion` 写入审计。

关键竞态是「`waiting_approval` 期间规则变了」。审批应答到达时**必须用当前快照重算**：

| 重算结果 | 处理 |
|---|---|
| 新规则要求 `Denied` | 提案作废，返回 `policy_changed`，交回模型重提 |
| 新规则仍要求审批 | 继续等待，TUI 提示理由已变更 |
| 新规则不再要求审批 | **不采纳自动放行**，仍按原审批走完 |

第三种情况之所以自动成立，是因为外部规则本来就无权放宽。

## Considered Options

- **允许插件返回完整 `Decision`（含 `Auto`）** — 表达能力最强，但等于把唯一安全边界交给外部代码，审计只能记录「规则说可以」。
- **远程 feed 可以放宽** — 更新最省事，但 feed 或签名密钥一旦失守就是后门，且很难在事后发现。
- **只支持本地文件、不做 feed** — 最简单，但 CVE 情报到落地要等人工改配置重启，与初衷不符。
- **每次工具调用现查远程** — 规则最新，但引入网络延迟与可用性依赖，工具调用路径不能这么设计。

## Consequences

- v1 做：内嵌默认规则 + `policy.d/*.yaml` + 不可变 `Ruleset` + `PolicyVersion` 入审计，约半天；后者事后补不了，所以必须现在做。
- v2 做：签名 feed、热更新、TUI stale 提示，约 2–3 天。单用户场景无 PKI 需求，公钥固定本地即可。
- 放宽权限永远需要人工介入，牺牲一点灵活性，换「任何放行都有本地动作可追溯」。
- 每次决策对全部规则做一次匹配，规则量必须设上限，否则工具调用延迟随 feed 增长。
- 规则只能在「判定依据」上扩展；要改引擎的短路顺序或新增判定层，必须改代码并发新 ADR。
