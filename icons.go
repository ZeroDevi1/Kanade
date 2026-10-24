package main

import "github.com/egoist/mygo/ui"

// icon 把 16×16 线条图形包装成 SVG；描边用 currentColor，随文字颜色变化。
func icon(shapes string) *ui.SVG {
	return ui.MustParseSVG([]byte(`<svg xmlns="http://www.w3.org/2000/svg" viewBox="0 0 16 16" fill="none" stroke="currentColor" stroke-width="1.2" stroke-linecap="round" stroke-linejoin="round">` + shapes + `</svg>`))
}

var (
	iconMenu     = icon(`<path d="M2.5 4h11M2.5 8h11M2.5 12h11"/>`)
	iconHome     = icon(`<path d="M2.5 7 8 2.5 13.5 7v6.5h-3.5V10h-4v3.5H2.5z"/>`)
	iconCantus   = icon(`<rect x="6" y="1.5" width="4" height="8" rx="2"/><path d="M3.5 7.5a4.5 4.5 0 0 0 9 0M8 12v2.5"/>`)
	iconTimbre   = icon(`<path d="M1.5 8h2l1.5-4 2 8 2-10 2 9 1.5-3h2"/>`)
	iconStems    = icon(`<path d="M2 4h12M2 8h12M2 12h12"/><circle cx="5" cy="4" r="1.2"/><circle cx="10" cy="8" r="1.2"/><circle cx="7" cy="12" r="1.2"/>`)
	iconScore    = icon(`<path d="M6 12.5V3l7-1.5v9.5"/><circle cx="4.5" cy="12.5" r="1.5"/><circle cx="11.5" cy="11" r="1.5"/>`)
	iconTasks    = icon(`<path d="M5.5 4h8M5.5 8h8M5.5 12h8"/><path d="M2 4l.8.8L4 3.5M2 8l.8.8L4 7.5M2 12l.8.8L4 11.5"/>`)
	iconLab      = icon(`<path d="M6 1.5v5L2.5 13a1 1 0 0 0 .9 1.5h9.2a1 1 0 0 0 .9-1.5L10 6.5v-5M5 1.5h6M4.5 10h7"/>`)
	iconExpand   = icon(`<path d="M9.5 2.5h4v4M6.5 13.5h-4v-4M13.5 2.5 9 7M2.5 13.5 7 9"/>`)
	iconSettings = icon(`<circle cx="8" cy="8" r="2"/><path d="M8 1.5v2M8 12.5v2M1.5 8h2M12.5 8h2M3.4 3.4l1.4 1.4M11.2 11.2l1.4 1.4M3.4 12.6l1.4-1.4M11.2 4.8l1.4-1.4"/>`)
)
