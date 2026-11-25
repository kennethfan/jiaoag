//go:build !darwin && !linux

package exec

import (
	"context"
	"fmt"
)

// NewSandbox 在尚未实现隔离后端的平台上明确失败，而不是假装隔离。
func NewSandbox() (*Sandbox, error) {
	return nil, fmt.Errorf("%w: 当前平台没有 sandbox 后端", ErrNotAvailable)
}

// Sandbox 在未支持平台上只是为了让接口与调用方编译通过；NewSandbox 会先失败，
// 正常路径不会拿到实例。
type Sandbox struct{}

func (b *Sandbox) Name() string { return "sandbox" }

func (b *Sandbox) Prepare(context.Context, Command) (Plan, error) {
	return Plan{}, fmt.Errorf("%w: 当前平台没有 sandbox 后端", ErrNotAvailable)
}

func (b *Sandbox) Start(Plan, Spec) (*Process, error) {
	return nil, ErrNotAvailable
}

func (b *Sandbox) Run(context.Context, Plan, Spec) (Result, error) {
	return Result{}, fmt.Errorf("%w: 当前平台没有 sandbox 后端", ErrNotAvailable)
}
