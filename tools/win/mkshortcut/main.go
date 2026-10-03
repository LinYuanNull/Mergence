// mkshortcut 生成 Windows 桌面快捷方式（纯 Go，绕开被策略拦截的 COM/VBS 路径）。
//
// 用法: mkshortcut <目标exe> <lnk输出路径> <工作目录>
package main

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"

	ole "github.com/go-ole/go-ole"
	shortcut "github.com/nyaosorg/go-windows-shortcut"
)

func main() {
	// go-ole 需要显式初始化 COM 单元，否则 CreateShortcut 会报
	// "尚未调用 CoInitialize"。
	if err := ole.CoInitializeEx(0, ole.COINIT_APARTMENTTHREADED); err != nil {
		fmt.Fprintln(os.Stderr, "CoInitializeEx:", err)
		os.Exit(1)
	}
	defer ole.CoUninitialize()

	if len(os.Args) < 4 {
		fmt.Fprintln(os.Stderr, "usage: mkshortcut <targetExe> <lnkPath> <workingDir>")
		os.Exit(2)
	}
	target, lnk, wd := os.Args[1], os.Args[2], os.Args[3]

	lnkAbs, err := filepath.Abs(lnk)
	if err != nil {
		fmt.Fprintln(os.Stderr, "resolve lnk path:", err)
		os.Exit(1)
	}
	if !strings.HasSuffix(strings.ToLower(lnkAbs), ".lnk") {
		lnkAbs += ".lnk"
	}
	_ = os.Remove(lnkAbs)

	if err := shortcut.Make(target, lnkAbs, wd); err != nil {
		fmt.Fprintln(os.Stderr, "Make failed:", err)
		os.Exit(1)
	}

	// 读回校验，确认链接真的指向目标而不是写了个空壳。
	tp, rwd, rerr := shortcut.Read(lnkAbs)
	fmt.Printf("lnk       = %s\n", lnkAbs)
	fmt.Printf("target    = %s\n", target)
	fmt.Printf("workdir   = %s\n", wd)
	fmt.Printf("readback  = target:%q workdir:%q err:%v\n", tp, rwd, rerr)

	if !strings.EqualFold(filepath.Clean(tp), filepath.Clean(target)) {
		fmt.Fprintln(os.Stderr, "WARN: 读回的目标路径与预期不一致")
		os.Exit(3)
	}
	fmt.Println("OK")
}
