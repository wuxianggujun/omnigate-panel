//go:build !linux

// secret_other.go 非 Linux 平台的密码读取回退：带回显读取。生产部署在 Linux 容器内，
// 命令行设密请在容器里执行（那里走 secret_linux.go 的无回显实现）。
package main

import (
	"fmt"
	"os"
)

// readSecret 打印提示并读取一行（非 Linux 平台无 termios，密码会回显）。
func readSecret(prompt string) (string, error) {
	fmt.Fprint(os.Stderr, prompt)
	return readLinePlain()
}
