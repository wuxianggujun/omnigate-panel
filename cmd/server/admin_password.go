// admin_password.go 命令行面板设密（-set-admin-password）：首次引导或忘记密码时的
// 兜底入口，直接写入 PostgreSQL panel_users，不启动 HTTP 服务。
//
// 为什么要有它：面板账号与网关 api_key 解耦后，若把唯一管理员密码忘了——或首次
// 部署还没建账号——就没法从面板里自救。命令行入口保证你能在服务器上重新拿回控制权。
package main

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"os"
	"strings"

	"github.com/wuxianggujun/omnigate-panel/internal/panelauth"
	"github.com/wuxianggujun/omnigate-panel/internal/pgstore"
)

// runSetAdminPassword 交互读取密码并写入 PostgreSQL panel_users。返回后由调用方
// 决定退出。数据库是面板账号的唯一权威，config.json 不再保存密码哈希。
func runSetAdminPassword(pg *pgstore.Store, username, role string) error {
	role = strings.TrimSpace(role)
	if !panelauth.ValidRole(panelauth.Role(role)) {
		return fmt.Errorf("非法角色 %q（仅 admin/viewer）", role)
	}
	username = strings.TrimSpace(username)
	if username == "" {
		return errors.New("账号名不能为空")
	}
	if pg == nil {
		return errors.New("PostgreSQL 未连接，无法写入面板账号")
	}
	pw, err := promptPassword()
	if err != nil {
		return err
	}
	ctx := context.Background()
	users, _, err := pg.LoadPanelUsers(ctx)
	if err != nil {
		return fmt.Errorf("读取面板账号失败: %w", err)
	}
	users, err = panelauth.UpsertUser(users, username, panelauth.Role(role), pw)
	if err != nil {
		return err
	}
	if err := pg.SavePanelUsers(ctx, users); err != nil {
		return fmt.Errorf("写入面板账号失败: %w", err)
	}
	fmt.Fprintf(os.Stderr, "已写入 PostgreSQL panel_users：账号 %q（角色 %s）。重启面板进程后生效。\n", username, role)
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
