package agent

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/kennethfan/jiaoag/internal/domain"
	"github.com/kennethfan/jiaoag/internal/patch"
	"github.com/kennethfan/jiaoag/internal/tools"
)

// 写入审批模式。与 config.Agent.WriteApproval 取值一致；这里再声明一次是为了
// 让 agent 包不依赖配置层，也便于库的零值（""）明确落回最安全的一档。
const (
	WriteApprovalWorkspace = "workspace"
	WriteApprovalAlways    = "always"
	WriteApprovalOff       = "off"

	// auditWriteAuto 记「没有经过人工审批的写入」。和 write.applied 分开，
	// 是为了让审计能直接回答「哪些改动没人看过」。
	auditWriteAuto = "write.auto"
)

// autoWrite 判定这次写入是否可以跳过人工审批。返回的 reason 会进审计，
// 说明放行依据（workspace / tmp / off）。
//
// 判定顺序：模式 → 是否落在允许的根内 → 是否命中敏感路径。任何一步不确定
// 都返回 false，让调用方走原来的审批流。
func (a *Agent) autoWrite(change *tools.Change) (bool, string) {
	switch a.opts.WriteApproval {
	case WriteApprovalOff:
		return true, "write_approval=off"
	case WriteApprovalWorkspace:
	default:
		// "" 与 always 都按「必须审批」处理。
		return false, ""
	}

	target, rel := a.changeTarget(change)
	abs, err := target.Resolve(rel)
	if err != nil {
		return false, ""
	}
	checked := resolveWritePath(abs)
	root, kind, ok := matchRoot(checked, a.writeRoots())
	if !ok {
		return false, ""
	}
	if sensitiveWrite(filepath.ToSlash(strings.TrimPrefix(checked, root+string(filepath.Separator)))) {
		return false, ""
	}
	return true, kind
}

// writeRoots 返回允许免审写入的根：会话的全部工作区根，加上临时目录。
// 工作区根在 workspace.New 时已经解析过符号链接，这里只需去重。
func (a *Agent) writeRoots() []string {
	var roots []string
	if a.opts.Workspace != nil {
		roots = append(roots, a.opts.Workspace.Root())
	}
	if a.opts.Roots != nil {
		for _, extra := range a.opts.Roots.Extra() {
			roots = append(roots, extra.Path)
		}
	}
	roots = append(roots, tempWriteRoots()...)
	seen := make(map[string]bool, len(roots))
	out := roots[:0]
	for _, r := range roots {
		r = filepath.Clean(r)
		if r == "" || seen[r] {
			continue
		}
		seen[r] = true
		out = append(out, r)
	}
	return out
}

// tempWriteRoots 是免审写入允许的临时目录，均已解析符号链接：macOS 上
// /tmp 与 /var 都是 /private/* 的软链，不归一化会对不上。
func tempWriteRoots() []string {
	candidates := []string{os.TempDir(), "/tmp", "/private/tmp", "/var/tmp", "/private/var/tmp"}
	var out []string
	seen := make(map[string]bool, len(candidates))
	for _, dir := range candidates {
		if dir == "" {
			continue
		}
		real := dir
		if resolved, err := filepath.EvalSymlinks(dir); err == nil {
			real = resolved
		}
		if seen[real] {
			continue
		}
		seen[real] = true
		out = append(out, real)
	}
	return out
}

// resolveWritePath 尽量把目标路径的符号链接解开。文件可能还不存在，此时
// 退一步解析父目录——否则「工作区里放个指向 /etc 的软链」就能绕过判定。
func resolveWritePath(abs string) string {
	if real, err := filepath.EvalSymlinks(abs); err == nil {
		return real
	}
	dir, base := filepath.Split(filepath.Clean(abs))
	if real, err := filepath.EvalSymlinks(dir); err == nil {
		return filepath.Join(real, base)
	}
	return filepath.Clean(abs)
}

// matchRoot 找 path 所属的最深根（工作区可能就落在 /tmp 下），并给出放行
// 依据。path 与 roots 都必须已经归一化。
func matchRoot(path string, roots []string) (root, kind string, ok bool) {
	for _, candidate := range roots {
		rel, err := filepath.Rel(candidate, path)
		if err != nil || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) || filepath.IsAbs(rel) {
			continue
		}
		if len(candidate) <= len(root) {
			continue
		}
		root = candidate
	}
	if root == "" {
		return "", "", false
	}
	for _, dir := range tempWriteRoots() {
		if root == dir {
			return root, "tmp", true
		}
	}
	return root, "workspace", true
}

// sensitiveWrite 判断根内相对路径是否属于「改坏了会静默出事」的那一类。
// 这些路径即使在 workspace 模式下也保留审批。
func sensitiveWrite(rel string) bool {
	parts := strings.Split(rel, "/")
	for _, part := range parts {
		switch part {
		case ".git", ".jiaoag", ".agents":
			return true
		}
	}
	lower := strings.ToLower(rel)
	if strings.HasPrefix(lower, ".github/workflows/") {
		return true
	}
	base := parts[len(parts)-1]
	switch base {
	case "AGENTS.md", "MEMORY.md", ".npmrc", ".netrc", ".git-credentials", ".pgpass":
		return true
	}
	// 构建入口：make 已在命令白名单里免审，如果 Makefile 也能静默改写，
	// 就等于「改脚本 → 自动执行」的免审链路。改它必须让人看 diff。
	if base == "Makefile" || base == "makefile" || base == "GNUmakefile" ||
		strings.HasSuffix(strings.ToLower(base), ".mk") {
		return true
	}
	if base == ".env" || strings.HasPrefix(base, ".env.") {
		return true
	}
	for _, prefix := range []string{"id_rsa", "id_dsa", "id_ecdsa", "id_ed25519"} {
		if strings.HasPrefix(base, prefix) {
			return true
		}
	}
	for _, suffix := range []string{".pem", ".p12", ".pfx", ".keystore", ".jks", ".kdbx", ".ppk"} {
		if strings.HasSuffix(strings.ToLower(base), suffix) {
			return true
		}
	}
	return false
}

// applyAutoChange 是不经审批的写入路径：事件、审计、CAS 一个不少，只是没有
// 等待裁决这一步。proposal.created 带上 diff，事后仍能回答改了哪些行。
func (a *Agent) applyAutoChange(ctx context.Context, task domain.Task, toolCall *domain.ToolCall,
	prepared tools.Prepared, reason string) (tools.Result, error) {

	change := prepared.Change
	proposalID, err := domain.NewID("prp")
	if err != nil {
		return tools.Result{}, err
	}
	proposal := &domain.Proposal{
		ID:         domain.ProposalID(proposalID),
		TaskID:     task.ID,
		ToolCallID: toolCall.ID,
		Path:       change.Path,
		Diff:       change.Diff,
		BaseHash:   change.BaseHash,
		NewHash:    change.NewHash,
		Status:     domain.ProposalPending,
	}

	created, err := domain.NewEvent(domain.EventProposalCreated, task.SessionID, task.ID,
		domain.ProposalCreatedPayload{
			ProposalID: proposal.ID,
			Path:       change.Path,
			Diff:       change.Diff,
			BaseHash:   change.BaseHash,
		})
	if err != nil {
		return tools.Result{}, err
	}

	target, rel := a.changeTarget(change)
	hash, exists, err := target.Hash(rel)
	if err != nil {
		return a.failAutoChange(ctx, task, toolCall, proposal, change, created,
			fmt.Sprintf("检查 %s 的当前内容失败：%v", change.Path, err))
	}
	if exists != change.Exists || hash != change.BaseHash {
		return a.failAutoChange(ctx, task, toolCall, proposal, change, created,
			fmt.Sprintf("stale_proposal：%s 在提出改动后已被修改过（期望 %s，实际 %s）。请重新 read_file 后再提出改动",
				change.Path, patch.ShortHash(change.BaseHash), patch.ShortHash(hash)))
	}
	if err := target.Write(rel, change.NewContent); err != nil {
		return a.failAutoChange(ctx, task, toolCall, proposal, change, created,
			fmt.Sprintf("写入 %s 失败：%v", change.Path, err))
	}

	proposal.Status = domain.ProposalApplied
	proposal.AppliedAt = domain.Now()
	proposal.NewHash = change.NewHash
	toolCall.Status = domain.ToolSucceeded

	applied, err := domain.NewEvent(domain.EventProposalApplied, task.SessionID, task.ID,
		domain.ProposalAppliedPayload{
			ProposalID: proposal.ID,
			Path:       change.Path,
			NewHash:    change.NewHash,
			Auto:       true,
			Mode:       reason,
		})
	if err != nil {
		return tools.Result{}, err
	}
	// 磁盘已经变了：这条结局必须落库，哪怕任务在同一瞬间被取消。
	// 免审路径从不离开 running：不设 To，否则会写成一次非法的 running→running。
	if err := a.record(context.WithoutCancel(ctx), task, domain.Transition{
		ToolCall: toolCall,
		Proposal: proposal,
		Audit:    []domain.AuditRecord{autoWriteAudit(auditWriteAuto, toolCall, change, reason, "")},
		Events:   []domain.Event{created, applied},
	}); err != nil {
		return tools.Result{}, err
	}
	return tools.Result{Content: describeAuto(change, reason)}, nil
}

func (a *Agent) failAutoChange(ctx context.Context, task domain.Task, toolCall *domain.ToolCall,
	proposal *domain.Proposal, change *tools.Change, created domain.Event, message string) (tools.Result, error) {

	proposal.Status = domain.ProposalFailed
	toolCall.Status = domain.ToolFailed
	rejected, err := rejectedEvent(task, proposal, message)
	if err != nil {
		return tools.Result{}, err
	}
	if err := a.record(context.WithoutCancel(ctx), task, domain.Transition{
		ToolCall: toolCall,
		Proposal: proposal,
		Audit:    []domain.AuditRecord{autoWriteAudit(auditWriteFailed, toolCall, change, "", message)},
		Events:   []domain.Event{created, rejected},
	}); err != nil {
		return tools.Result{}, err
	}
	return tools.Result{Content: message + "；文件未被修改", IsError: true}, nil
}

// autoWriteAudit 记录免审写入。actor 固定为 policy：没有人参与这次放行，
// 把它记成 user 会让审计撒谎。
func autoWriteAudit(action string, toolCall *domain.ToolCall, change *tools.Change,
	reason, errText string) domain.AuditRecord {

	detail := map[string]any{
		"path":      change.Path,
		"base_hash": change.BaseHash,
		"new_hash":  change.NewHash,
		"bytes":     len(change.NewContent),
		"auto":      true,
	}
	if change.Root != "" {
		detail["root"] = change.Root
	}
	if reason != "" {
		detail["mode"] = reason
	}
	if errText != "" {
		detail["reason"] = errText
	}
	raw, err := json.Marshal(detail)
	if err != nil {
		raw = nil
	}
	return domain.AuditRecord{
		Actor:      string(domain.SourcePolicy),
		Action:     action,
		ToolCallID: toolCall.ID,
		Detail:     raw,
	}
}

func describeAuto(c *tools.Change, reason string) string {
	verb := "已修改"
	if !c.Exists {
		verb = "已新建"
	}
	where := "工作区内"
	if reason == "tmp" {
		where = "临时目录内"
	} else if strings.HasPrefix(reason, "write_approval=") {
		where = "按配置免审"
	}
	return fmt.Sprintf("%s %s（%s自动应用，内容摘要 %s）", verb, c.Path, where, patch.ShortHash(c.NewHash))
}
