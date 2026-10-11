package main

import (
	"reflect"
	"testing"

	omniconfig "github.com/wuxianggujun/omnigate-panel/internal/omnigate/config"
)

// TestDropAPIKey 面板密钥必须能从面向客户端的列表里摘干净：历史版本把它持久化进了
// api_keys，不摘的话它会以「一把全量客户端密钥」的形态出现在面板密钥卡里，轮换后也
// 仍继续可用（文件里还留着）。空 key 原样返回，绝不能把整张表清空。
func TestDropAPIKey(t *testing.T) {
	in := omniconfig.APIKeyList{
		{Key: "sk-panel"},
		{Key: "sk-client", Name: "客户端", Providers: []string{"raccoon"}},
	}
	got := dropAPIKey(in, "sk-panel")
	want := omniconfig.APIKeyList{{Key: "sk-client", Name: "客户端", Providers: []string{"raccoon"}}}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("dropAPIKey=%+v want %+v", got, want)
	}
	if got := dropAPIKey(in, ""); len(got) != len(in) {
		t.Fatalf("空 key 必须原样返回，得到 %+v", got)
	}
	if got := dropAPIKey(in, "sk-not-there"); len(got) != len(in) {
		t.Fatalf("未命中的 key 不该改动列表，得到 %+v", got)
	}
	if got := dropAPIKey(nil, "sk-panel"); len(got) != 0 {
		t.Fatalf("空列表应保持为空，得到 %+v", got)
	}
}
