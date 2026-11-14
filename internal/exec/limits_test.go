package exec

import "testing"

// 这部分与平台无关：校验与支持矩阵本身不该依赖 GOOS。

func TestLimitsValidate(t *testing.T) {
	cases := []struct {
		name string
		l    Limits
		ok   bool
	}{
		{"零值不限制", Limits{}, true},
		{"三项都合法", Limits{CPUSeconds: 60, MemoryMB: 512, MaxFiles: 256}, true},
		{"CPU 为负", Limits{CPUSeconds: -1}, false},
		{"内存为负", Limits{MemoryMB: -1}, false},
		{"句柄为负", Limits{MaxFiles: -1}, false},
		{"内存太小", Limits{MemoryMB: 32}, false},
		{"内存刚好到下限", Limits{MemoryMB: MinMemoryMB}, true},
	}
	for _, c := range cases {
		err := c.l.Validate()
		if c.ok && err != nil {
			t.Fatalf("%s：不该报错，得到 %v", c.name, err)
		}
		if !c.ok && err == nil {
			t.Fatalf("%s：应当报错", c.name)
		}
	}
}

func TestLimitsIsZeroAndMissing(t *testing.T) {
	if !(Limits{}).IsZero() {
		t.Fatal("零值应视为不限制")
	}
	if (Limits{CPUSeconds: 1}).IsZero() {
		t.Fatal("设置了 CPU 就不算零值")
	}

	all := Support{CPUSeconds: true, MemoryMB: true, MaxFiles: true}
	if miss := all.Missing(Limits{CPUSeconds: 1, MemoryMB: 64, MaxFiles: 16}); len(miss) != 0 {
		t.Fatalf("全支持时不应有缺失项，得到 %v", miss)
	}
	none := Support{}
	if miss := none.Missing(Limits{CPUSeconds: 1, MemoryMB: 64}); len(miss) != 2 {
		t.Fatalf("全不支持时应列出两项，得到 %v", miss)
	}
	// 没请求的项不算「缺失」。
	if miss := none.Missing(Limits{}); len(miss) != 0 {
		t.Fatalf("未请求限制时不应有缺失项，得到 %v", miss)
	}
}
