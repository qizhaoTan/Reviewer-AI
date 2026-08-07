package tool

import (
	"context"
	"encoding/json"
	"fmt"

	"github.com/qizhaoTan/Reviewer-AI/internal/schema"
)

// ITool 是所有具体工具必须实现的通用接口。
//
// 引入这一层的原因：随着工具数量增加，main.go 里原先靠 "工具列表 + switch tc.Name"
// 两处手动维护的分发方式，每加一个新工具都要同时改两处，容易漏改。改成接口后
// main.go 只需持有一个 []ITool，新增工具只需实现接口、加入该 slice。
type ITool interface {
	// Definition 返回用于提交给大模型的工具元信息和参数 JSON Schema，
	// 其中 Definition().Name 就是工具的全局唯一名称 (大模型通过这个名字调用它)。
	//
	// 工具名故意不再单独开一个 Name() 方法：如果 Name() 和 Definition().Name
	// 各自维护，两者就可能被改出不一致（比如只改了其中一处），
	// 用同一个方法作为唯一数据源可以从根上避免这种问题。
	Definition() schema.ToolDefinition

	// Execute 接收大模型吐出的 JSON 参数，执行具体业务逻辑
	// 注意：参数是 json.RawMessage，反序列化由各个具体工具内部自行处理
	Execute(ctx context.Context, repoRoot string, args json.RawMessage) Result
}

// FindToolByName 在 tools 中查找 Definition().Name 等于 name 的工具，找不到时返回 error。
func FindToolByName(tools []ITool, name string) (ITool, error) {
	for _, t := range tools {
		if t.Definition().Name == name {
			return t, nil
		}
	}
	return nil, fmt.Errorf("tool %q not found", name)
}
