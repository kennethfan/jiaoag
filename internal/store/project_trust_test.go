package store_test

import (
	"context"
	"strings"
	"testing"
)

func TestProjectTrustLifecycle(t *testing.T) {
	ctx := context.Background()
	s, _ := openStore(t)

	digest, err := s.ProjectTrust(ctx, "/tmp/ws")
	if err != nil {
		t.Fatalf("无记录不该报错：%v", err)
	}
	if digest != "" {
		t.Fatalf("无记录应返回空串，得到 %q", digest)
	}

	if err := s.TrustProject(ctx, "/tmp/ws", "d1"); err != nil {
		t.Fatalf("写入信任：%v", err)
	}
	digest, err = s.ProjectTrust(ctx, "/tmp/ws")
	if err != nil || digest != "d1" {
		t.Fatalf("读回信任失败：%q %v", digest, err)
	}

	// 内容变化后重新确认：同一行更新而不是插一行。
	if err := s.TrustProject(ctx, "/tmp/ws", "d2"); err != nil {
		t.Fatalf("更新信任：%v", err)
	}
	digest, err = s.ProjectTrust(ctx, "/tmp/ws")
	if err != nil || digest != "d2" {
		t.Fatalf("更新后应读到新摘要：%q %v", digest, err)
	}

	// 不同工作区互不影响。
	if err := s.TrustProject(ctx, "/tmp/other", "d3"); err != nil {
		t.Fatalf("写入第二个工作区：%v", err)
	}
	if digest, _ := s.ProjectTrust(ctx, "/tmp/ws"); digest != "d2" {
		t.Fatalf("工作区之间不该串：%q", digest)
	}

	if err := s.RevokeProjectTrust(ctx, "/tmp/ws"); err != nil {
		t.Fatalf("撤销：%v", err)
	}
	if digest, _ := s.ProjectTrust(ctx, "/tmp/ws"); digest != "" {
		t.Fatalf("撤销后应为空：%q", digest)
	}
	// 幂等：再撤一次不报错。
	if err := s.RevokeProjectTrust(ctx, "/tmp/ws"); err != nil {
		t.Fatalf("重复撤销：%v", err)
	}
}

func TestProjectTrustRejectsEmptyArguments(t *testing.T) {
	ctx := context.Background()
	s, _ := openStore(t)

	if err := s.TrustProject(ctx, "", "d"); err == nil {
		t.Fatal("空工作区应报错")
	}
	if err := s.TrustProject(ctx, "/tmp/ws", ""); err == nil {
		t.Fatal("空摘要应报错：否则等于无条件信任")
	}
	if err := s.RevokeProjectTrust(ctx, ""); err == nil {
		t.Fatal("空工作区撤销应报错")
	}
}

func TestProjectTrustNeedsMigration3(t *testing.T) {
	s, path := openStore(t)
	if s == nil {
		t.Fatal("应打开成功")
	}
	// 表已由迁移建好；这里验证 probe 之后仍能正常查询（schema 校验没有误报）。
	if !strings.HasSuffix(path, ".db") {
		t.Fatalf("临时库路径异常：%q", path)
	}
	ctx := context.Background()
	if _, err := s.ProjectTrust(ctx, "/tmp/ws"); err != nil {
		t.Fatalf("查询应可用：%v", err)
	}
}
