package main

import (
	"errors"
	"fmt"
	"os/exec"
)

func main() {
	cmd := exec.Command("false") // 一个必定以退出码 1 结束的命令
	err := cmd.Run()

	// 旧写法：先声明目标类型的指针变量，再取地址传给 errors.As。
	// go run golang.org/x/tools/gopls/internal/analysis/modernize/cmd/modernize@latest ./cmd/demo-errorsastype/...
	// 进行诊断，可以提示出最新的写法
	var exitErr *exec.ExitError
	if errors.As(err, &exitErr) {
		fmt.Println("exit code:", exitErr.ExitCode())
	}
}
