package geminiweb

import (
	"context"
	"encoding/json"
	"sort"
	"strings"
	"time"

	"github.com/wuxianggujun/omnigate-panel/internal/omnigate/provider/geminiweb/fingerprint"
)

// WebChat 是账号在 Gemini 网页端的一条会话记录。
type WebChat struct {
	ID     string  `json:"id"`
	Title  string  `json:"title"`
	Pinned bool    `json:"pinned"`
	TS     float64 `json:"ts"`
}

// CleanupResult 汇总一次会话清理的结果。
type CleanupResult struct {
	Listed  int `json:"listed"`
	Deleted int `json:"deleted"`
	Skipped int `json:"skipped"`
	Failed  int `json:"failed"`
}

const (
	// chatListPageDefault 是 list_web_chats 单轮拉取上限（与参考实现一致）。
	chatListPageDefault = 300
	// chatCleanupMaxRounds 是单账号单次清理的安全轮数上限（每轮最多一页），
	// 防止重度账号无限循环。
	chatCleanupMaxRounds = 50
	// chatCleanupMinKeepHours 是保留窗口下界：至少保留 1 小时，避免误传 0/负值
	// 把账号会话清空。
	chatCleanupMinKeepHours = 1.0
	// chatDeleteGap 是两次删除之间的轻微间隔，避免触发上游限流。
	chatDeleteGap = 300 * time.Millisecond
)

// listWebChats 列出账号的网页端会话（MaZiqc / LIST_CHATS）。参考实现发两次
// （flag 1 与 0）再合并去重；每项解析 cid=[0]、title=[1]、pinned=[2]、
// ts=[5]([秒,纳秒])。返回按 ts 降序。
func (c *Client) listWebChats(ctx context.Context, jar *fingerprint.Jar, sessionToken string, recent int) ([]WebChat, error) {
	if recent <= 0 {
		recent = chatListPageDefault
	}
	seen := map[string]WebChat{}
	var firstErr error
	for _, flag := range []int{1, 0} {
		payload, err := json.Marshal([]any{recent, nil, []any{flag, nil, 1}})
		if err != nil {
			return nil, err
		}
		raw, err := c.batchexecute(ctx, jar, sessionToken, "MaZiqc", string(payload))
		if err != nil {
			if firstErr == nil {
				firstErr = err
			}
			continue
		}
		parseChats(parseWrbBody(raw, "MaZiqc"), seen)
	}
	if len(seen) == 0 && firstErr != nil {
		return nil, firstErr
	}
	out := make([]WebChat, 0, len(seen))
	for _, ch := range seen {
		out = append(out, ch)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].TS > out[j].TS })
	return out, nil
}

// deleteWebChat 删除一个网页端会话（两步 RPC：GzXR5e -> qWymEb，两步都要执行）。
// cid 必须是 c_xxx 形式；两步都拿到 200 才算成功。
func (c *Client) deleteWebChat(ctx context.Context, jar *fingerprint.Jar, sessionToken, cid string) (bool, error) {
	if !strings.HasPrefix(cid, "c_") {
		return false, nil
	}
	p1, err := json.Marshal([]any{cid})
	if err != nil {
		return false, err
	}
	if _, err := c.batchexecute(ctx, jar, sessionToken, "GzXR5e", string(p1)); err != nil {
		return false, err
	}
	p2, err := json.Marshal([]any{cid, []any{1, nil, 0, 1}})
	if err != nil {
		return false, err
	}
	if _, err := c.batchexecute(ctx, jar, sessionToken, "qWymEb", string(p2)); err != nil {
		return false, err
	}
	return true, nil
}

// cleanupWebChats 清理超过 keepHours 的网页端会话（置顶可选跳过）。返回
// {listed, deleted, skipped, failed}。
//
// 循环「拉取 + 删除」直到本轮没有可删的为止：list_web_chats 单轮有上限，重度账号
// 堆积可能超过上限，最旧的会话排在列表外永远清不到，所以每轮删完重新拉。
func (c *Client) cleanupWebChats(ctx context.Context, jar *fingerprint.Jar, sessionToken string, keepHours float64, skipPinned bool) (CleanupResult, error) {
	if keepHours < chatCleanupMinKeepHours {
		keepHours = chatCleanupMinKeepHours
	}
	var res CleanupResult
	for round := 0; round < chatCleanupMaxRounds; round++ {
		chats, err := c.listWebChats(ctx, jar, sessionToken, chatListPageDefault)
		if err != nil {
			return res, err
		}
		if len(chats) == 0 {
			break
		}
		cutoff := float64(time.Now().Unix()) - keepHours*3600
		roundDeleted := 0
		for _, chat := range chats {
			if skipPinned && chat.Pinned {
				res.Skipped++
				continue
			}
			// ts<=0 说明时间戳没解析出来，保守跳过（避免误删）。
			if chat.TS <= 0 || chat.TS >= cutoff {
				res.Skipped++
				continue
			}
			ok, _ := c.deleteWebChat(ctx, jar, sessionToken, chat.ID)
			if ok {
				res.Deleted++
				roundDeleted++
			} else {
				res.Failed++
			}
			if !pauseCtx(ctx, chatDeleteGap) {
				res.Listed += len(chats)
				return res, ctx.Err()
			}
		}
		res.Listed += len(chats)
		// 本轮没删任何会话（剩下的要么置顶、要么在保留窗口内）→ 没有更多可清的。
		if roundDeleted == 0 {
			break
		}
	}
	return res, nil
}

// parseChats 把 MaZiqc 的已解码 body 合并进 into（按 cid 去重，先到先得）。
func parseChats(body any, into map[string]WebChat) {
	root, ok := body.([]any)
	if !ok || len(root) < 3 {
		return
	}
	list, ok := root[2].([]any)
	if !ok {
		return
	}
	for _, raw := range list {
		chat, ok := raw.([]any)
		if !ok || len(chat) == 0 {
			continue
		}
		cid, ok := chat[0].(string)
		if !ok {
			continue
		}
		if _, seen := into[cid]; seen {
			continue
		}
		c := WebChat{ID: cid}
		if len(chat) > 1 {
			c.Title, _ = chat[1].(string)
		}
		if len(chat) > 2 {
			c.Pinned = truthy(chat[2])
		}
		if len(chat) > 5 {
			if ts, ok := chat[5].([]any); ok && len(ts) > 0 {
				sec, _ := ts[0].(float64)
				var nanos float64
				if len(ts) > 1 {
					nanos, _ = ts[1].(float64)
				}
				c.TS = sec + nanos/1e9
			}
		}
		into[cid] = c
	}
}

// truthy 复刻 Python 的 bool()：非零数字或 true 为真。
func truthy(v any) bool {
	switch x := v.(type) {
	case bool:
		return x
	case float64:
		return x != 0
	case string:
		return x != ""
	}
	return false
}

// pauseCtx 睡眠 d，ctx 取消时提前返回 false。
func pauseCtx(ctx context.Context, d time.Duration) bool {
	if d <= 0 {
		return true
	}
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return false
	case <-t.C:
		return true
	}
}
