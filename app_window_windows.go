package main

import (
	"syscall"
	"unsafe"
)

var (
	procDwmSetWindowAttribute = syscall.NewLazyDLL("dwmapi.dll").NewProc("DwmSetWindowAttribute")
	user32                    = syscall.NewLazyDLL("user32.dll")
	procGetWindowLongPtrW     = user32.NewProc("GetWindowLongPtrW")
	procSetWindowLongPtrW     = user32.NewProc("SetWindowLongPtrW")
	procSetWindowPos          = user32.NewProc("SetWindowPos")
)

const (
	gwlStyle   = ^uintptr(15) // GWL_STYLE（-16）
	wsSysMenu  = 0x00080000
	swpRestyle = 0x0001 | 0x0002 | 0x0004 | 0x0010 | 0x0020 // NOSIZE | NOMOVE | NOZORDER | NOACTIVATE | FRAMECHANGED
)

// setSystemMenu 给窗口加上或去掉窗口菜单（WS_SYSMENU）。
//
// Mica 窗口的框架延伸到整个客户区（MyGo 的 SetVibrancy），最大化时 DWM 会在这片框架上画出系统自己的
// 最小化 / 还原 / 关闭按钮，与 MyGo 自绘的窗口按钮错开几个像素叠在一起（Windows 11 实测）。
// DWM 只给带窗口菜单的窗口画这组按钮，因此最大化期间去掉 WS_SYSMENU、还原时加回：
// 自绘按钮经 WM_SYSCOMMAND 最小化 / 还原 / 关闭，不依赖窗口菜单（实测不受影响）；
// 代价是最大化时 Alt+空格不弹出窗口菜单。MyGo 若在框架层解决此问题，可删除这一处理。
func setSystemMenu(hwnd uintptr, on bool) {
	if hwnd == 0 {
		return
	}
	style, _, _ := procGetWindowLongPtrW.Call(hwnd, gwlStyle)
	next := style &^ wsSysMenu
	if on {
		next = style | wsSysMenu
	}
	if next == style {
		return
	}
	procSetWindowLongPtrW.Call(hwnd, gwlStyle, next)
	procSetWindowPos.Call(hwnd, 0, 0, 0, 0, 0, swpRestyle)
}

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
