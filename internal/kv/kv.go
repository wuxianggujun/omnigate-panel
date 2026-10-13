// Package kv 定义「键 → 字节块」的极简存储后端接口，供面板各子系统把本地 JSON
// 状态搬到 PostgreSQL（或任何实现该接口的后端）时复用。
//
// 约定：
//   - Load 未命中返回 (nil, false, nil)；后端不可用返回 (nil, false, err)；
//   - Save 覆盖写（不存在则插入）；
//   - 实现必须并发安全。
//
// 本包是叶子包（不 import 任何本项目内部包），避免与 pgstore / panelauth 等形成
// 导入环：pgstore 实现 Backend，各子系统只依赖本接口。
package kv

import "errors"

// ErrUnavailable 表示后端当前不可用（连不上库等）。调用方据此决定是否回落本地文件。
var ErrUnavailable = errors.New("kv: backend unavailable")

// Backend 字节块存储。
type Backend interface {
	Load(key string) ([]byte, bool, error)
	Save(key string, data []byte) error
	Delete(key string) error
}
