# ADR-0043：构建/打包类命令进白名单，高风险子命令仍然审批

- 状态：已采纳（v2.41）
- 日期：2026-11-25

## 背景

命令白名单最初只放行 `go test/vet/run`、`ls`、`cat`、`head`、`wc`。`go build` 被刻意排除，理由是它会把二进制写进当前目录，等于绕过了「写入必须审批」。

v2.40（ADR-0042）把工作区与临时目录的写入改成免审之后，这条理由不再成立：`go build` 的产物落在工作区里，与一次被自动应用的 `write_file` 同级；而日常开发里「构建/打包」是最高频的动作，每次都弹审批会把审批疲劳请回来。

同时，威胁模型被明确为「防误操作」（用户在 v2.41 设计中选定），而不是「防仓库内容不可信」。这意味着按命令名一刀切地拦构建没有收益——它既不读凭据，也不对外发布。

## 决策

1. **程序级白名单**新增 `go`（补 `build`/`mod`）、`npm`、`pnpm`、`mvn`/`mvnw`、`gradle`/`gradlew`、`make`。仍然只认 `argv[0]` 是程序名本身，带路径一律审批。
2. **不设黑名单**：子命令不枚举全集的程序（npm/pnpm/mvn/gradle/make）允许任意子命令。
3. **高风险子命令不禁止，但固定落到审批**，理由写成 `risky_subcommand: <程序> <子命令> 会对外发布或操作凭据…`。集合：
   - npm/pnpm：`publish`、`unpublish`、`exec`、`x`、`dlx`、`login`、`adduser`、`logout`、`token`、`owner`、`access`、`deprecate`、`dist-tag`、`deploy`
   - mvn：`deploy`、`release`、`versions:set`（`release:prepare` 这类插件目标按前缀命中）
   - gradle：`publish`、`uploadArchives`、`deploy`
   - make：`install`、`uninstall`
4. **会改变「读哪个文件 / 写到哪里」的 flag 继续硬拒**：`go -o/-toolexec/-exec/-pkgdir`、`make -f/-C/--eval`、`mvn -f/-s/-Dmaven.repo.local`、`gradle -p/--init-script/-g`、`npm -g/--prefix/--registry/--userconfig`。
5. 规则集版本升到 `builtin-3`；`risk_subcommands` 进入规则 hash，因此 `policy_hash` 与 `extensions_digest` 会随这次策略变化而变。
6. **`make` 免审的配套约束**：`Makefile`、`makefile`、`GNUmakefile`、`*.mk` 加入 ADR-0042 的敏感写入清单，改构建入口仍要人看 diff；否则「写 Makefile → 下一轮 `make` 自动执行」就是免审的任意代码链路。

## 后果

- `npm install` 会执行依赖包的 `postinstall`、`mvn test` 会加载并执行插件——这是威胁模型 (a) 的自然后果，写进文档，不假装它安全。
- `go build` 会在工作区留下二进制，但没有 `proposal.created` 事件（它不是写入工具）。谁构建了什么，只能从 `exec.allow/exec.approved` 的审计回答。
- 新增程序时只需加一条规则 + 一组高风险子命令；判定逻辑不再按命令名特判。
- 误操作的兜底仍靠审计与 `git`：免审不等于无痕。

## 备选方案

- **程序级记忆（首次确认后同程序免审）**：被否。审批状态要落库、要按工作区隔离，复杂度远高于静态规则，而规则本身随版本可控。
- **只放行 `go build`**：被否。日常里 `npm run build`、`make build` 同样高频，只解决一个语言等于把问题留给下个项目。
- **按目录判断（cwd 在工作区内就免审）**：被否。`rm -rf ~` 的 cwd 也在工作区内，等于没有判据。
- **高风险子命令直接拒绝**：被否。用户明确选择「不设黑名单，有风险提示用户审批」——发布有时真的要发，只是不能悄悄发。