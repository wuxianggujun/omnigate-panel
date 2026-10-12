package geminiweb

import (
	"context"
	"encoding/json"
	"strings"

	"github.com/wuxianggujun/omnigate-panel/internal/omnigate/provider/geminiweb/fingerprint"
)

// Gem 是账号的一个自定义 Gem（保存下来的人格：名称 + 可选描述 + 背后的提示词）。
// 移植自 gclient.py 的 list_gems / create_gem / update_gem / delete_gem。
type Gem struct {
	ID          string `json:"id"`
	Name        string `json:"name"`
	Description string `json:"description"`
	Prompt      string `json:"prompt"`
}

// gemListPayload 选择「用户自定义 Gem」：payload [2,["en"],0]。其它索引是预置
// Gem（[3..]/[4..]），网关不需要。
const gemListPayload = `[2,["en"],0]`

// listGems 拉取账号的自定义 Gem（CNgdBe / LIST_GEMS）。响应 body[2] 是 Gem 列表：
// gem[0]=id、gem[1][0]=名称、gem[1][1]=描述、gem[2][0]=提示词（可能缺省）。
func (c *Client) listGems(ctx context.Context, jar *fingerprint.Jar, sessionToken string) ([]Gem, error) {
	raw, err := c.batchexecute(ctx, jar, sessionToken, "CNgdBe", gemListPayload)
	if err != nil {
		return nil, err
	}
	return parseGems(parseWrbBody(raw, "CNgdBe")), nil
}

// createGem 新建一个自定义 Gem（oMH3Zd），成功返回新 Gem 的 id，否则返回 ""。
func (c *Client) createGem(ctx context.Context, jar *fingerprint.Jar, sessionToken, name, prompt, description string) (string, error) {
	payload, err := json.Marshal([]any{[]any{
		name, description, prompt,
		nil, nil, nil, nil, nil, 0, nil, 1, nil, nil, nil, []any{},
	}})
	if err != nil {
		return "", err
	}
	raw, err := c.batchexecute(ctx, jar, sessionToken, "oMH3Zd", string(payload))
	if err != nil {
		return "", err
	}
	if body, ok := parseWrbBody(raw, "oMH3Zd").([]any); ok && len(body) > 0 {
		if id, ok := body[0].(string); ok {
			return id, nil
		}
	}
	return "", nil
}

// updateGem 修改一个自定义 Gem（kHv0Vd）。拿到 200 即视为成功（与参考实现一致）。
func (c *Client) updateGem(ctx context.Context, jar *fingerprint.Jar, sessionToken, gemID, name, prompt, description string) (bool, error) {
	if strings.TrimSpace(gemID) == "" {
		return false, nil
	}
	payload, err := json.Marshal([]any{gemID, []any{
		name, description, prompt,
		nil, nil, nil, nil, nil, 0, nil, 1, nil, nil, nil, []any{}, 0,
	}})
	if err != nil {
		return false, err
	}
	if _, err := c.batchexecute(ctx, jar, sessionToken, "kHv0Vd", string(payload)); err != nil {
		return false, err
	}
	return true, nil
}

// deleteGem 删除一个自定义 Gem（UXcSJb）。拿到 200 即视为成功。
func (c *Client) deleteGem(ctx context.Context, jar *fingerprint.Jar, sessionToken, gemID string) (bool, error) {
	if strings.TrimSpace(gemID) == "" {
		return false, nil
	}
	payload, err := json.Marshal([]any{gemID})
	if err != nil {
		return false, err
	}
	if _, err := c.batchexecute(ctx, jar, sessionToken, "UXcSJb", string(payload)); err != nil {
		return false, err
	}
	return true, nil
}

// parseGems 从 CNgdBe 的已解码 body 里读出 Gem 列表。结构异常时返回 nil。
func parseGems(body any) []Gem {
	root, ok := body.([]any)
	if !ok || len(root) < 3 {
		return nil
	}
	list, ok := root[2].([]any)
	if !ok {
		return nil
	}
	out := make([]Gem, 0, len(list))
	for _, raw := range list {
		gem, ok := raw.([]any)
		if !ok || len(gem) == 0 {
			continue
		}
		id, ok := gem[0].(string)
		if !ok {
			continue
		}
		g := Gem{ID: id}
		if len(gem) > 1 {
			if meta, ok := gem[1].([]any); ok {
				if len(meta) > 0 {
					g.Name, _ = meta[0].(string)
				}
				if len(meta) > 1 {
					g.Description, _ = meta[1].(string)
				}
			}
		}
		if len(gem) > 2 {
			if p, ok := gem[2].([]any); ok && len(p) > 0 {
				g.Prompt, _ = p[0].(string)
			}
		}
		out = append(out, g)
	}
	return out
}
