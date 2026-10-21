package main

import (
	"syscall"
	"unsafe"
)

var procDwmSetWindowAttribute = syscall.NewLazyDLL("dwmapi.dll").NewProc("DwmSetWindowAttribute")

const (
	// dwmwaCaptionColor 与 dwmColorNone 见 DWMWINDOWATTRIBUTE / DWMWA_COLOR_NONE（Windows 11 22000 起支持，更早的系统调用失败、忽略即可）。
	dwmwaCaptionColor = 35
	dwmColorNone      = 0xFFFFFFFE
)

// clearCaptionAccent 让 DWM 不给隐藏的标题栏上色。
//
// 隐藏标题栏的窗口在 Windows 11 上仍保留 1 px 非客户区（MyGo 为保留贴靠布局），Mica 又把框架延伸进整个客户区；
// 系统设置开启「在标题栏和窗口边框上显示强调色」时，DWM 会把这部分标题栏涂成强调色，
// 表现为窗口顶部一条强调色（最大化时整条自绘标题栏区域都是强调色）。边框颜色不受影响，与其他窗口一致。
func clearCaptionAccent(hwnd uintptr) {
	if hwnd == 0 || procDwmSetWindowAttribute.Find() != nil {
		return
	}
	color := uint32(dwmColorNone)
	procDwmSetWindowAttribute.Call(hwnd, dwmwaCaptionColor, uintptr(unsafe.Pointer(&color)), 4)
}
