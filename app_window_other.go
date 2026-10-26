//go:build !windows

package main

// clearCaptionAccent 与 setSystemMenu 只在 Windows 上有意义（见 app_window_windows.go）。
func clearCaptionAccent(uintptr) {}

func setSystemMenu(uintptr, bool) {}
