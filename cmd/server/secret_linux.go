//go:build linux

// secret_linux.go 终端无回显读密码（Linux）。用 stdlib syscall 直接操作 termios，
// 不引任何外部依赖（生产部署在 Linux 容器内执行命令行设密）。非终端（管道）或
// ioctl 失败时退化为带回显读取。
package main

import (
	"bufio"
	"fmt"
	"os"
	"strings"
	"syscall"
	"unsafe"
)

// readSecret 打印提示并关闭终端回显读取一行密码。
func readSecret(prompt string) (string, error) {
	fd := int(os.Stdin.Fd())
	old, err := ioctlGetTermios(fd)
	if err != nil {
		// 非终端：退化为普通读取。
		fmt.Fprint(os.Stderr, prompt)
		return readLinePlain()
	}
	fmt.Fprint(os.Stderr, prompt)
	mod := *old
	mod.Lflag &^= syscall.ECHO
	if err := ioctlSetTermios(fd, &mod); err != nil {
		return readLinePlain()
	}
	defer func() {
		_ = ioctlSetTermios(fd, old)
		fmt.Fprintln(os.Stderr)
	}()
	line, err := bufio.NewReader(os.Stdin).ReadString('\n')
	if err != nil && line == "" {
		return "", err
	}
	return strings.TrimRight(line, "\r\n"), nil
}

func ioctlGetTermios(fd int) (*syscall.Termios, error) {
	var t syscall.Termios
	if _, _, errno := syscall.Syscall(syscall.SYS_IOCTL, uintptr(fd),
		uintptr(syscall.TCGETS), uintptr(unsafe.Pointer(&t))); errno != 0 {
		return nil, errno
	}
	return &t, nil
}

func ioctlSetTermios(fd int, t *syscall.Termios) error {
	if _, _, errno := syscall.Syscall(syscall.SYS_IOCTL, uintptr(fd),
		uintptr(syscall.TCSETS), uintptr(unsafe.Pointer(t))); errno != 0 {
		return errno
	}
	return nil
}
