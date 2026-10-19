package config

import "testing"

func TestValidateRejectsNonHTTPProviderURL(t *testing.T) {
	c := &Config{Providers: []ProviderConfig{{
		Name: "x", Type: "openai", BaseURL: "file:///etc/passwd",
	}}}
	c.applyDefaults()
	if err := c.validate(); err == nil {
		t.Fatal("file:// 上游地址应被拒绝")
	}
}

func TestValidateAcceptsHTTPProviderURL(t *testing.T) {
	c := &Config{Providers: []ProviderConfig{{
		Name: "x", Type: "openai", BaseURL: "https://example.com/v1",
	}}}
	c.applyDefaults()
	if err := c.validate(); err != nil {
		t.Fatalf("https 上游地址应通过: %v", err)
	}
}

func TestValidateRejectsNonHTTPCheckinURL(t *testing.T) {
	c := &Config{
		Providers: []ProviderConfig{{Name: "x", Type: "openai", BaseURL: "https://example.com/v1"}},
		Checkin: CheckinConfig{Tasks: []CheckinTask{{
			Name: "task", Provider: "x", Type: "http", URL: "gopher://example.com/",
		}}},
	}
	c.applyDefaults()
	if err := c.validate(); err == nil {
		t.Fatal("非 http/https 签到 URL 应被拒绝")
	}
}
