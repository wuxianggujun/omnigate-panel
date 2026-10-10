// admin_password.go 命令行面板设密（-set-admin-password）：首次引导或忘记密码时的
// 兜底入口，直接改写 config.json 的 panel_auth.users，不启动 HTTP 服务。
//
// 为什么要有它：面板账号与网关 api_key 解耦后，若把唯一管理员密码忘了——或首次
// 部署还没建账号——就没法从面板里自救。命令行入口保证你能在服务器上重新拿回控制权。
package main

import (
	"bufio"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"strings"

	"github.com/wuxianggujun/omnigate-panel/internal/panelauth"
)

// runSetAdminPassword 交互读取密码并写入配置。返回后由调用方决定退出。
func runSetAdminPassword(cfgPath, username, role string) error {
	role = strings.TrimSpace(role)
	if !panelauth.ValidRole(panelauth.Role(role)) {
		return fmt.Errorf("非法角色 %q（仅 admin/viewer）", role)
	}
	username = strings.TrimSpace(username)
	if username == "" {
		return errors.New("账号名不能为空")
	}
	// 配置文件不存在时先落一份推荐配置（含随机 api_key），与首次启动行为一致。
	if _, err := os.Stat(cfgPath); errors.Is(err, fs.ErrNotExist) {
		if _, werr := WriteDefault(cfgPath); werr != nil {
			return fmt.Errorf("生成默认配置失败: %w", werr)
		}
		fmt.Fprintf(os.Stderr, "已生成 %s\n", cfgPath)
	}
	pw, err := promptPassword()
	if err != nil {
		return err
	}
	if err := SetAdminPassword(cfgPath, username, panelauth.Role(role), pw); err != nil {
		return err
	}
	fmt.Fprintf(os.Stderr, "已写入 %s：账号 %q（角色 %s）。重启面板进程后生效。\n", cfgPath, username, role)
	return nil
}

// SetAdminPassword 把某个账号（新增或重置）写入 config.json，保留文件中其它字段
// 与既有账号。写入采用「临时文件 + 重命名」尽量规避半截写坏配置。
func SetAdminPassword(path, username string, role panelauth.Role, password string) error {
	raw, err := os.ReadFile(path)
	if err != nil {
		return err
	}
	var m map[string]any
	if err := json.Unmarshal(raw, &m); err != nil {
		return fmt.Errorf("解析 %s: %w", path, err)
	}
	var section PanelAuthConfig
	if pa, ok := m["panel_auth"]; ok && pa != nil {
		b, _ := json.Marshal(pa)
		if err := json.Unmarshal(b, &section); err != nil {
			return fmt.Errorf("解析 panel_auth: %w", err)
		}
	}
	users, err := panelauth.UpsertUser(section.Users, username, role, password)
	if err != nil {
		return err
	}
	if section.SessionHours <= 0 {
		section.SessionHours = 72
	}
	if section.MaxFailures <= 0 {
		section.MaxFailures = 5
	}
	if section.LockMinutes <= 0 {
		section.LockMinutes = 15
	}
	m["panel_auth"] = map[string]any{
		"session_hours": section.SessionHours,
		"max_failures":  section.MaxFailures,
		"lock_minutes":  section.LockMinutes,
		"users":         users,
	}
	out, err := json.MarshalIndent(m, "", "  ")
	if err != nil {
		return err
	}
	out = append(out, '\n')
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, out, 0o600); err != nil {
		return fmt.Errorf("写临时配置: %w", err)
	}
	if err := os.Rename(tmp, path); err != nil {
		// Windows 下目标存在时 Rename 会失败：先删再重命名。
		if os.Remove(path) == nil {
			if err2 := os.Rename(tmp, path); err2 == nil {
				return nil
			}
		}
		return fmt.Errorf("落盘配置: %w", err)
	}
	return nil
}

// promptPassword 读取新密码。stdin 是终端时连读两次比对（无回显，见 readSecret）；
// 否则（管道）只读一行，便于脚本：echo 'pw' | omnigate-panel -set-admin-password。
func promptPassword() (string, error) {
	fi, _ := os.Stdin.Stat()
	if fi == nil || fi.Mode()&os.ModeCharDevice == 0 {
		pw, err := readLinePlain()
		if err != nil {
			return "", err
		}
		if pw == "" {
			return "", errors.New("密码不能为空")
		}
		return pw, nil
	}
	p1, err := readSecret("请输入新密码: ")
	if err != nil {
		return "", err
	}
	if p1 == "" {
		return "", errors.New("密码不能为空")
	}
	p2, err := readSecret("请再次输入新密码: ")
	if err != nil {
		return "", err
	}
	if p1 != p2 {
		return "", errors.New("两次输入不一致")
	}
	return p1, nil
}

// readLinePlain 从 stdin 读一行并去掉行尾换行（管道输入用）。
func readLinePlain() (string, error) {
	line, err := bufio.NewReader(os.Stdin).ReadString('\n')
	if err != nil && line == "" {
		return "", err
	}
	return strings.TrimRight(line, "\r\n"), nil
}
