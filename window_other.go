//go:build !windows

package main

// clearCaptionAccent 只在 Windows 上有意义（见 window_windows.go）。
func clearCaptionAccent(uintptr) {}
