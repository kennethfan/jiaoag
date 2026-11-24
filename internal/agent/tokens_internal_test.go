package agent

import (
	"strings"
	"testing"

	"github.com/kennethfan/jiaoag/internal/llm"
)

func TestEstimateTextWeighsCJKAndASCII(t *testing.T) {
	ascii := estimateText(strings.Repeat("a", 400))
	cjk := estimateText(strings.Repeat("中", 100))
	if ascii < 80 || ascii > 130 {
		t.Fatalf("400 个 ASCII 字符应估算在 100 token 附近，得到 %d", ascii)
	}
	if cjk < 100 || cjk > 120 {
		t.Fatalf("100 个汉字应估算在 100 token 附近，得到 %d", cjk)
	}
	// 同样的字符数，中文明显更贵——这正是按字符配预算失真的原因。
	if cjk <= ascii/2 {
		t.Fatalf("中文估算 %d 不应低于 ASCII %d 的一半", cjk, ascii)
	}
	if estimateText("") != 0 || estimateText("a") < 1 {
		t.Fatalf("空串估算应为 0，非空至少为 1")
	}
}

func TestEstimatorCalibratesAgainstRealUsage(t *testing.T) {
	msgs := []llm.Message{llm.TextMessage(llm.RoleUser, strings.Repeat("a", 4000))}
	est := newTokenEstimator()
	before := est.messages(msgs)
	if before <= 0 {
		t.Fatalf("估算值应为正")
	}

	// 真实用量是估算的两倍：系数应上移，同样的消息估算变大。
	est.observe(before, before*2)
	after := est.messages(msgs)
	if after <= before {
		t.Fatalf("校准后估算应变大：before=%d after=%d", before, after)
	}

	// 反复观测极端偏差也不会越过上限。
	for i := 0; i < 50; i++ {
		est.observe(after, after*100)
	}
	if est.ratio > maxTokenRatio {
		t.Fatalf("校准系数应为上限 %.1f 以内，得到 %.2f", maxTokenRatio, est.ratio)
	}
}

func TestEstimatorIgnoresTinyRequests(t *testing.T) {
	est := newTokenEstimator()
	est.observe(10, 500)
	if est.ratio != defaultTokenRatio {
		t.Fatalf("小请求不应参与校准，ratio=%.2f", est.ratio)
	}
	est.observe(minCalibrationTokens, 0)
	if est.ratio != defaultTokenRatio {
		t.Fatalf("零用量不应参与校准，ratio=%.2f", est.ratio)
	}
}

func TestOverheadCountsSystemAndTools(t *testing.T) {
	est := newTokenEstimator()
	without := est.overhead("短的系统提示", nil)
	withTools := est.overhead("短的系统提示", []llm.ToolSpec{{
		Name:        "read_file",
		Description: strings.Repeat("读取文件内容。", 40),
		Schema:      []byte(`{"type":"object","properties":{"path":{"type":"string"}}}`),
	}})
	if withTools <= without {
		t.Fatalf("工具清单应计入固定开销：without=%d with=%d", without, withTools)
	}
}

// Build 的预算单位是 token：校准后的估算变贵时，同样的预算应丢掉更多轮次。
func TestBuildUsesCalibratedEstimator(t *testing.T) {
	history := []llm.Message{
		llm.TextMessage(llm.RoleUser, strings.Repeat("a", 400)),
		llm.TextMessage(llm.RoleAssistant, strings.Repeat("b", 400)),
		llm.TextMessage(llm.RoleUser, strings.Repeat("c", 400)),
	}
	est := newTokenEstimator()
	budget := est.messages(history) / 2

	kept := buildWith(history, budget, est)
	if len(kept) >= len(history) {
		t.Fatalf("预算只有一半时应丢轮次，实际保留 %d 条", len(kept))
	}

	// 系数放大到上限后，同样的预算只能留下最后一轮。
	for i := 0; i < 50; i++ {
		est.observe(budget, budget*100)
	}
	kept = buildWith(history, budget, est)
	if len(kept) != 1 {
		t.Fatalf("校准放大后应只保留最后一条用户消息，实际 %d 条", len(kept))
	}
}
