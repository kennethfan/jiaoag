package tools

import (
	"context"
	"encoding/json"
	"strings"

	"github.com/kennethfan/jiaoag/internal/skills"
)

const skillVersion = "1"

type skillArgs struct {
	Name string `json:"name"`
}

// NewSkill 返回按需加载技能正文的工具。
//
// 技能索引由宿主注入系统提示，模型只需要在这里要正文。加载是纯读取，
// 不产生副作用，也不需要审批——它和 read_file 是同一类操作，区别只是
// 路径被钉在技能目录里，模型给不了任意路径。
func NewSkill(cat *skills.Catalog) Tool { return &skillTool{catalog: cat} }

type skillTool struct {
	catalog *skills.Catalog
}

func (t *skillTool) Spec() Spec {
	return Spec{
		Name: "skill",
		Description: "加载一个技能包的完整说明。系统提示里只列了技能名与适用场景；" +
			"当任务与某个技能相符时，先用这个工具取正文再按步骤操作。可以多次加载不同技能。",
		Version: skillVersion,
		Effects: []Effect{EffectRead},
		Schema: json.RawMessage(`{
  "type": "object",
  "properties": {
    "name": {"type": "string", "description": "技能名，取自系统提示里的技能索引"}
  },
  "required": ["name"]
}`),
	}
}

func (t *skillTool) Prepare(ctx context.Context, call Call) (Prepared, error) {
	var args skillArgs
	if err := decodeArgs(call, &args); err != nil {
		return Prepared{}, err
	}
	name := strings.TrimSpace(args.Name)
	if name == "" {
		return Prepared{}, invalidArgs(call.Name, "name 不能为空")
	}
	if _, ok := t.catalog.Get(name); !ok {
		// 把可用名字回给模型，让它自己纠正，而不是让它去猜。
		names := t.catalog.Names()
		if len(names) > 20 {
			names = append(names[:20], "…")
		}
		return Prepared{}, invalidArgs(call.Name, "没有技能 %q；可用：%s", name, strings.Join(names, ", "))
	}
	return Prepared{Call: call, Spec: t.Spec(), Args: args}, nil
}

func (t *skillTool) Execute(ctx context.Context, p Prepared) (Result, error) {
	args, _ := p.Args.(skillArgs)
	body, err := t.catalog.Body(strings.TrimSpace(args.Name))
	if err != nil {
		return Result{IsError: true, Content: "加载技能失败：" + err.Error()}, nil
	}
	return Result{Content: body}, nil
}
