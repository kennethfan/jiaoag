package tools_test

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/kennethfan/jiaoag/internal/tools"
	"github.com/kennethfan/jiaoag/internal/workspace"
)

func writeRegistry(t *testing.T) (*tools.Registry, *workspace.Workspace) {
	t.Helper()
	ws, err := workspace.New(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	reg, err := tools.NewRegistry(tools.NewWriteFile(ws), tools.NewEditFile(ws))
	if err != nil {
		t.Fatal(err)
	}
	return reg, ws
}

// Prepare 只产出提案：此刻磁盘必须原封不动。
func TestWriteFilePrepareDoesNotTouchDisk(t *testing.T) {
	reg, ws := writeRegistry(t)
	writeFile(t, ws, "a.txt", "old\n")

	prepared, err := reg.Prepare(context.Background(), tools.Call{
		ID: "c1", Name: "write_file", Args: jsonArgs(`{"path":"a.txt","content":"new\n"}`),
	})
	if err != nil {
		t.Fatalf("Prepare: %v", err)
	}
	if prepared.Change == nil {
		t.Fatal("写入工具必须给出 Change")
	}
	if prepared.Change.Exists != true {
		t.Fatal("Change.Exists 应为 true")
	}
	if prepared.Change.BaseHash == "" || prepared.Change.NewHash == "" {
		t.Fatal("提案必须带新旧摘要")
	}
	if prepared.Change.BaseHash == prepared.Change.NewHash {
		t.Fatal("内容变了，摘要不该相同")
	}
	if !strings.Contains(prepared.Change.Diff, "+new") {
		t.Fatalf("diff 缺少新增行：\n%s", prepared.Change.Diff)
	}

	got, err := os.ReadFile(filepath.Join(ws.Root(), "a.txt"))
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != "old\n" {
		t.Fatalf("Prepare 阶段不该写盘，实际内容 %q", got)
	}
}

func TestWriteFilePrepareNewFileHasNoBaseHash(t *testing.T) {
	reg, ws := writeRegistry(t)
	prepared, err := reg.Prepare(context.Background(), tools.Call{
		ID: "c1", Name: "write_file", Args: jsonArgs(`{"path":"b.txt","content":"hi\n"}`),
	})
	if err != nil {
		t.Fatalf("Prepare: %v", err)
	}
	if prepared.Change.Exists {
		t.Fatal("新文件 Exists 应为 false")
	}
	if prepared.Change.BaseHash != "" {
		t.Fatalf("新文件不该有 base_hash，实际 %q", prepared.Change.BaseHash)
	}
	if !strings.Contains(prepared.Change.Diff, "/dev/null") {
		t.Fatalf("新建文件的 diff 头应指向 /dev/null：\n%s", prepared.Change.Diff)
	}
	if _, err := os.Stat(filepath.Join(ws.Root(), "b.txt")); !os.IsNotExist(err) {
		t.Fatal("Prepare 不该创建文件")
	}
}

func TestWriteFileNoChangeIsRejected(t *testing.T) {
	reg, ws := writeRegistry(t)
	writeFile(t, ws, "a.txt", "same\n")
	_, err := reg.Prepare(context.Background(), tools.Call{
		ID: "c1", Name: "write_file", Args: jsonArgs(`{"path":"a.txt","content":"same\n"}`),
	})
	if err == nil {
		t.Fatal("内容没变化时不该产出提案")
	}
	if !strings.Contains(err.Error(), "没有变化") {
		t.Fatalf("错误信息应说明原因，实际 %v", err)
	}
}

// 行级 diff 表达不了的改动不能进审批：用户看不见，就无从批准。
func TestWriteToolsRefuseInvisibleDiff(t *testing.T) {
	reg, ws := writeRegistry(t)
	// 没有结尾换行的文件：只补换行时 diff 会是空的。
	writeFile(t, ws, "noeol.txt", "abc")

	cases := []struct {
		name string
		call tools.Call
		want string
	}{
		{
			name: "write_file 只补结尾换行",
			call: tools.Call{ID: "c1", Name: "write_file",
				Args: jsonArgs(`{"path":"noeol.txt","content":"abc\n"}`)},
			want: "只差结尾换行",
		},
		{
			name: "edit_file 只补结尾换行",
			call: tools.Call{ID: "c2", Name: "edit_file",
				Args: jsonArgs(`{"path":"noeol.txt","old_string":"abc","new_string":"abc\n"}`)},
			want: "只差结尾换行",
		},
		{
			name: "新建空文件",
			call: tools.Call{ID: "c3", Name: "write_file",
				Args: jsonArgs(`{"path":"empty.txt","content":""}`)},
			want: "新建空文件",
		},
	}
	for _, c := range cases {
		_, err := reg.Prepare(context.Background(), c.call)
		if err == nil {
			t.Fatalf("%s: 空 diff 不该产出提案", c.name)
		}
		if !strings.Contains(err.Error(), c.want) {
			t.Fatalf("%s: 错误信息应说明原因，实际 %v", c.name, err)
		}
	}
	if _, err := os.Stat(filepath.Join(ws.Root(), "empty.txt")); !os.IsNotExist(err) {
		t.Fatal("被拒绝时不该创建文件")
	}
}

func TestEditFileUniqueMatch(t *testing.T) {
	reg, ws := writeRegistry(t)
	writeFile(t, ws, "a.go", "package main\n\nvar x = 1\n")

	prepared, err := reg.Prepare(context.Background(), tools.Call{
		ID: "c1", Name: "edit_file",
		Args: jsonArgs(`{"path":"a.go","old_string":"var x = 1","new_string":"var x = 2"}`),
	})
	if err != nil {
		t.Fatalf("Prepare: %v", err)
	}
	if got := string(prepared.Change.NewContent); got != "package main\n\nvar x = 2\n" {
		t.Fatalf("新内容 = %q", got)
	}
	if !strings.Contains(prepared.Change.Diff, "-var x = 1") || !strings.Contains(prepared.Change.Diff, "+var x = 2") {
		t.Fatalf("diff 不对：\n%s", prepared.Change.Diff)
	}
}

func TestEditFileAmbiguousMatchExplainsCount(t *testing.T) {
	reg, ws := writeRegistry(t)
	writeFile(t, ws, "a.go", "x\nx\nx\n")

	_, err := reg.Prepare(context.Background(), tools.Call{
		ID: "c1", Name: "edit_file", Args: jsonArgs(`{"path":"a.go","old_string":"x","new_string":"y"}`),
	})
	if err == nil {
		t.Fatal("多处命中时应拒绝")
	}
	if !strings.Contains(err.Error(), "3 次") {
		t.Fatalf("错误信息应给出命中次数，实际 %v", err)
	}
}

func TestEditFileReplaceAll(t *testing.T) {
	reg, ws := writeRegistry(t)
	writeFile(t, ws, "a.go", "x\nx\n")

	prepared, err := reg.Prepare(context.Background(), tools.Call{
		ID: "c1", Name: "edit_file",
		Args: jsonArgs(`{"path":"a.go","old_string":"x","new_string":"y","replace_all":true}`),
	})
	if err != nil {
		t.Fatalf("Prepare: %v", err)
	}
	if got := string(prepared.Change.NewContent); got != "y\ny\n" {
		t.Fatalf("新内容 = %q", got)
	}
}

func TestEditFileNoMatchAndMissingFile(t *testing.T) {
	reg, ws := writeRegistry(t)
	writeFile(t, ws, "a.go", "hello\n")

	_, err := reg.Prepare(context.Background(), tools.Call{
		ID: "c1", Name: "edit_file", Args: jsonArgs(`{"path":"a.go","old_string":"bye","new_string":"hi"}`),
	})
	if err == nil || !strings.Contains(err.Error(), "没有找到") {
		t.Fatalf("不命中应报错，实际 %v", err)
	}

	_, err = reg.Prepare(context.Background(), tools.Call{
		ID: "c2", Name: "edit_file", Args: jsonArgs(`{"path":"nope.go","old_string":"a","new_string":"b"}`),
	})
	if err == nil || !strings.Contains(err.Error(), "write_file") {
		t.Fatalf("文件不存在应提示改用 write_file，实际 %v", err)
	}
}

func TestEditFileKeepsCRLF(t *testing.T) {
	reg, ws := writeRegistry(t)
	writeFile(t, ws, "a.txt", "one\r\ntwo\r\n")

	prepared, err := reg.Prepare(context.Background(), tools.Call{
		ID: "c1", Name: "edit_file",
		Args: jsonArgs(`{"path":"a.txt","old_string":"two","new_string":"TWO"}`),
	})
	if err != nil {
		t.Fatalf("Prepare: %v", err)
	}
	if got := string(prepared.Change.NewContent); got != "one\r\nTWO\r\n" {
		t.Fatalf("换行风格被破坏：%q", got)
	}
}

// Execute 对写入工具必须失败关闭：绕过审批直接执行是绝不允许的。
func TestWriteToolsRefuseDirectExecute(t *testing.T) {
	reg, ws := writeRegistry(t)
	for _, call := range []tools.Call{
		{ID: "c1", Name: "write_file", Args: jsonArgs(`{"path":"a.txt","content":"x"}`)},
		{ID: "c2", Name: "edit_file", Args: jsonArgs(`{"path":"a.txt","old_string":"a","new_string":"b"}`)},
	} {
		prepared, err := reg.Prepare(context.Background(), call)
		if call.Name == "edit_file" {
			// edit_file 在空工作区里会因为文件不存在而失败，这是预期。
			if err == nil {
				t.Fatalf("%s: 空文件不该准备成功", call.Name)
			}
			continue
		}
		if err != nil {
			t.Fatalf("%s: Prepare: %v", call.Name, err)
		}
		res, err := reg.Execute(context.Background(), prepared)
		if err != nil {
			t.Fatalf("%s: Execute: %v", call.Name, err)
		}
		if !res.IsError || !strings.Contains(res.Content, "审批") {
			t.Fatalf("%s: 直接 Execute 应失败关闭，实际 %+v", call.Name, res)
		}
		if _, err := os.Stat(filepath.Join(ws.Root(), "a.txt")); !os.IsNotExist(err) {
			t.Fatalf("%s: Execute 不该写盘", call.Name)
		}
	}
}
