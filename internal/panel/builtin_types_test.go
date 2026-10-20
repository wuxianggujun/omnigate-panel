package panel

import (
	"io/fs"
	"testing"
)

// 兜底元数据是生成产物：catalog 改动后必须重新生成，否则面板在接口失败时
// 会退回一份过期的 schema（历史上正是这样漂移的）。
func TestBuiltinTypesGeneratedFileUpToDate(t *testing.T) {
	want, err := BuiltinTypesJS()
	if err != nil {
		t.Fatal(err)
	}
	got, err := fs.ReadFile(webRoot, builtinTypesPath)
	if err != nil {
		t.Fatalf("读取 %s 失败（忘了 go generate ./internal/panel？）: %v", builtinTypesPath, err)
	}
	if string(got) != string(want) {
		t.Fatalf("%s 已过期；运行 go generate ./internal/panel 重新生成", builtinTypesPath)
	}
}
