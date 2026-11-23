package server

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/coder/websocket"

	"github.com/kennethfan/jiaoag/internal/app"
	"github.com/kennethfan/jiaoag/internal/config"
	"github.com/kennethfan/jiaoag/internal/domain"
	"github.com/kennethfan/jiaoag/internal/event"
	"github.com/kennethfan/jiaoag/internal/llm"
	"github.com/kennethfan/jiaoag/internal/protocol"
	"github.com/kennethfan/jiaoag/internal/store"
	"github.com/kennethfan/jiaoag/internal/tools"
	"github.com/kennethfan/jiaoag/internal/workspace"
)

// idleProvider 不会真的被调用：这里只验命令校验，不验模型行为。
type idleProvider struct{}

func (idleProvider) Chat(context.Context, llm.Request) (llm.Response, error) {
	return llm.Response{}, errors.New("测试不使用非流式路径")
}

func (idleProvider) ChatStream(context.Context, llm.Request, func(llm.Chunk) error) error {
	return errors.New("测试不该真的调用模型")
}

// newSkillServer 起一个配了技能目录的服务端，并配置最小运行时：
// task.submit 只有在运行时存在时才会解析技能，也就才能报「没有这个技能」。
func newSkillServer(t *testing.T, dirs []string, mode string) (*httptest.Server, *app.App) {
	t.Helper()
	cfg := &config.Config{
		Server:  config.Server{Listen: "127.0.0.1:0", Token: testToken},
		Storage: config.Storage{DBPath: t.TempDir() + "/jiaoag.db", OutputDir: t.TempDir()},
		Agent:   config.Agent{Skills: mode, SkillsDirs: dirs},
	}
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	srv, err := New(cfg, log, "test")
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	db, err := store.Open(context.Background(), cfg.Storage.DBPath)
	if err != nil {
		t.Fatalf("打开数据库: %v", err)
	}
	application := app.New(db, event.NewHub())
	srv.WithApp(application)
	if err := application.ConfigureRuntime(app.RuntimeDeps{
		Provider: func(domain.Session) (llm.Provider, string, error) {
			return idleProvider{}, "test-model", nil
		},
		Tools: func(ws *workspace.Workspace, _ tools.RuntimeEnv) (*tools.Registry, error) {
			return tools.NewRegistry(tools.NewReadFile(ws))
		},
		SkillDirs:     dirs,
		MaxRounds:     3,
		ContextBudget: 10000,
	}); err != nil {
		t.Fatalf("ConfigureRuntime: %v", err)
	}

	ts := httptest.NewServer(srv.Handler())
	t.Cleanup(func() {
		ts.Close()
		_ = application.Close()
	})
	return ts, application
}

func writeSkill(t *testing.T, lib, name, front string) {
	t.Helper()
	dir := filepath.Join(lib, name)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "SKILL.md"), []byte(front), 0o644); err != nil {
		t.Fatal(err)
	}
}

// helloConn 连上服务端并完成客户端握手（之后的命令才不会被协议检查挡掉）。
func helloConn(t *testing.T, ts *httptest.Server) *websocket.Conn {
	t.Helper()
	c := dial(t, wsURL(ts), testToken)
	_ = readEnvelope(t, c) // hello
	ch, err := protocol.New(protocol.TypeClientHello, protocol.ClientHelloPayload{
		ClientVersion:   "test-client",
		ProtocolVersion: protocol.Version,
	})
	if err != nil {
		t.Fatalf("构造 client.hello: %v", err)
	}
	write(t, c, ch)
	return c
}

func TestSkillListReturnsCatalogWithManualFlag(t *testing.T) {
	lib := t.TempDir()
	writeSkill(t, lib, "auto-one", "---\nname: auto-one\ndescription: 自动技能\n---\n\n正文\n")
	writeSkill(t, lib, "manual-one", "---\nname: manual-one\ndescription: 只手动\ndisable-model-invocation: true\n---\n\n正文\n")
	ts, application := newSkillServer(t, []string{lib}, config.SkillAuto)
	sess, err := application.CreateSession(context.Background(), t.TempDir(), "", "", "技能")
	if err != nil {
		t.Fatalf("创建会话: %v", err)
	}
	c := helloConn(t, ts)

	req, err := protocol.New(protocol.TypeSkillList, protocol.SkillListPayload{SessionID: string(sess.ID)})
	if err != nil {
		t.Fatalf("构造 skill.list: %v", err)
	}
	write(t, c, req)

	env := readEnvelope(t, c)
	if env.Type != protocol.TypeSkillListResult {
		t.Fatalf("回帧 = %s，期望 skill.list.result", env.Type)
	}
	var p protocol.SkillListResultPayload
	if err := env.Decode(&p); err != nil {
		t.Fatalf("解码结果: %v", err)
	}
	if len(p.Skills) != 2 {
		t.Fatalf("技能数 = %d，期望 2：%+v", len(p.Skills), p.Skills)
	}
	got := map[string]protocol.SkillInfo{}
	for _, s := range p.Skills {
		got[s.Name] = s
	}
	if !got["manual-one"].Manual {
		t.Fatalf("manual-one 应标记为仅手动：%+v", p.Skills)
	}
	if got["auto-one"].Manual {
		t.Fatalf("auto-one 不该是仅手动：%+v", p.Skills)
	}
}

func TestSkillListEmptyWhenDisabled(t *testing.T) {
	lib := t.TempDir()
	writeSkill(t, lib, "auto-one", "---\nname: auto-one\ndescription: 自动技能\n---\n\n正文\n")
	ts, application := newSkillServer(t, []string{lib}, config.SkillOff)
	sess, err := application.CreateSession(context.Background(), t.TempDir(), "", "", "技能")
	if err != nil {
		t.Fatalf("创建会话: %v", err)
	}
	c := helloConn(t, ts)

	req, err := protocol.New(protocol.TypeSkillList, protocol.SkillListPayload{SessionID: string(sess.ID)})
	if err != nil {
		t.Fatalf("构造 skill.list: %v", err)
	}
	write(t, c, req)

	env := readEnvelope(t, c)
	if env.Type != protocol.TypeSkillListResult {
		t.Fatalf("回帧 = %s，期望 skill.list.result", env.Type)
	}
	var p protocol.SkillListResultPayload
	if err := env.Decode(&p); err != nil {
		t.Fatalf("解码结果: %v", err)
	}
	if len(p.Skills) != 0 {
		t.Fatalf("关闭技能后不该列出技能：%+v", p.Skills)
	}
}

func TestTaskSubmitRejectsUnknownSkill(t *testing.T) {
	lib := t.TempDir()
	writeSkill(t, lib, "auto-one", "---\nname: auto-one\ndescription: 自动技能\n---\n\n正文\n")
	ts, application := newSkillServer(t, []string{lib}, config.SkillAuto)
	sess, err := application.CreateSession(context.Background(), t.TempDir(), "", "", "技能")
	if err != nil {
		t.Fatalf("创建会话: %v", err)
	}
	c := helloConn(t, ts)

	req, err := protocol.New(protocol.TypeTaskSubmit, protocol.TaskSubmitPayload{
		SessionID: string(sess.ID),
		Text:      "随便做点什么",
		Skills:    []string{"no-such-skill"},
	})
	if err != nil {
		t.Fatalf("构造 task.submit: %v", err)
	}
	write(t, c, req)

	env := readEnvelope(t, c)
	if env.Type != protocol.TypeError {
		t.Fatalf("回帧 = %s，期望 error", env.Type)
	}
	var p protocol.ErrorPayload
	if err := env.Decode(&p); err != nil {
		t.Fatalf("解码 error: %v", err)
	}
	if p.Code != protocol.CodeBadRequest {
		t.Fatalf("错误码 = %s，期望 %s", p.Code, protocol.CodeBadRequest)
	}
	if !strings.Contains(p.Message, "no-such-skill") {
		t.Fatalf("错误信息应带技能名：%q", p.Message)
	}
	// 校验失败不该留下任务。
	tasks, err := application.ListTasks(context.Background(), sess.ID)
	if err != nil {
		t.Fatalf("ListTasks: %v", err)
	}
	if len(tasks) != 0 {
		t.Fatalf("失败提交不应落库，得到 %d 个任务", len(tasks))
	}
}
