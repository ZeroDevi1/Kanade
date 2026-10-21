package main

import (
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/egoist/mygo"
	"github.com/egoist/mygo/ui"

	"kanade/internal/ui/fluent"
)

// result 是一个暂存区中的输出文件。暂存区在系统临时目录，应用退出时删除；用户点「保存…」才复制到磁盘。
type result struct {
	path  string // 暂存区中的文件
	title string // 列表中的显示名
	name  string // 另存时建议的文件名（不含扩展名）
	saved string // 最近一次另存的位置；空为尚未保存
	added time.Time
}

// resultSet 是一个页面的输出列表（最新在前）与预览。保存在 app 中，切换页面不丢失；只在 UI 线程修改。
type resultSet struct {
	items   []*result
	preview audioPreview
	err     string
}

// add 登记一个输出，show 为 true 时载入预览；同一文件只登记一次。返回是否为新登记。
func (rs *resultSet) add(a *app, path, title, name string, show bool) bool {
	if path == "" || rs.find(path) != nil {
		return false
	}
	if name == "" {
		name = strings.TrimSuffix(filepath.Base(path), filepath.Ext(path))
	}
	rs.items = append([]*result{{path: path, title: title, name: name, added: time.Now()}}, rs.items...)
	if show || rs.preview.path == "" {
		rs.preview.load(a, path)
	}
	return true
}

func (rs *resultSet) find(path string) *result {
	for _, r := range rs.items {
		if r.path == path {
			return r
		}
	}
	return nil
}

// show 构建预览（含保存按钮）与输出列表，点击列表项切换预览。
func (rs *resultSet) show(c *ui.Context, tk *fluent.Tokens, a *app, empty string) {
	t := c.Theme()
	cur := rs.find(rs.preview.path)
	rs.preview.show(c, tk, empty, func() {
		if cur == nil {
			return
		}
		if ui.PrimaryButton(c, "保存…").Tooltip("另存到磁盘，可修改文件名").Clicked() {
			a.saveResult(rs, cur)
		}
	})
	if cur != nil && cur.saved != "" {
		ui.Row(c).Gap(8).AlignItems(ui.Center).Children(func() {
			ui.Text(c, "已保存到 "+cur.saved).FontSize(12).TextColor(t.Success).Grow(1).SingleLine().Selectable()
			if ui.Button(c, "在文件夹中显示").Clicked() {
				dir := filepath.Dir(cur.saved)
				go func() { _ = mygo.Shell.OpenPath(dir) }()
			}
		})
	} else if cur != nil {
		ui.Text(c, "尚未保存：结果暂存在系统临时目录，退出 Kanade 后删除。").FontSize(12).TextColor(t.TextMuted)
	}
	if rs.err != "" {
		ui.Text(c, rs.err).FontSize(12).TextColor(t.Danger).Selectable()
	}
	if len(rs.items) < 2 {
		return
	}
	ui.Row(c).Gap(8).AlignItems(ui.Center).Children(func() {
		ui.Text(c, fmt.Sprintf("本次生成（%d）", len(rs.items))).FontSize(12).TextColor(t.TextMuted).Grow(1)
		if ui.Button(c, "全部保存…").Tooltip("选择一个文件夹，把列表中的全部结果按建议文件名复制进去").Clicked() {
			a.saveAllResults(rs)
		}
	})
	for _, r := range rs.items {
		label := r.title
		if label == "" {
			label = r.name
		}
		if r.saved != "" {
			label += " · 已保存"
		}
		b := ui.Button(c.Key("result-"+r.path), label).Tooltip(r.name)
		if r.path == rs.preview.path {
			b = b.Disabled(true)
		}
		if b.Clicked() {
			rs.preview.load(a, r.path)
		}
	}
}

// saveResult 打开保存对话框，把 r 复制到用户选择的位置。
func (a *app) saveResult(rs *resultSet, r *result) {
	a.saveFileAs(r.path, r.name, func(dst string, err error) {
		rs.err = errText(err)
		if err == nil {
			r.saved = dst
		}
	})
}

// saveAllResults 选择一个文件夹，把 rs 中的全部结果复制进去；文件名为各自的建议名，与已有文件重名时加序号，不覆盖。
// 逐个复制，某个失败时继续其余文件，最后在 rs.err 汇总失败项；已成功的项记录保存位置。
func (a *app) saveAllResults(rs *resultSet) {
	type job struct {
		r              *result
		src, name, dst string
		err            error
	}
	jobs := make([]*job, len(rs.items))
	for i, r := range rs.items {
		jobs[i] = &job{r: r, src: r.path, name: safeFileName(r.name) + filepath.Ext(r.path)}
	}
	dir := a.saveDir
	if dir == "" {
		dir = a.be.layout.Outputs
	}
	win := a.win
	go func() {
		dirs, err := mygo.Dialog.Open(mygo.OpenDialogOptions{
			Parent: win, Title: "全部保存到文件夹", DefaultPath: dir, Directory: true, CreateDirectories: true,
		})
		if err != nil || len(dirs) == 0 {
			return
		}
		dst := dirs[0]
		var failed []string
		for _, j := range jobs {
			// uniquePath 依据磁盘现状去重，须在上一个文件写完后再算下一个，同名结果才不会互相覆盖。
			j.dst = uniquePath(dst, j.name)
			if j.err = copyFileAtomic(j.src, j.dst); j.err != nil {
				failed = append(failed, j.name+"："+j.err.Error())
			}
		}
		a.post(func() {
			a.saveDir = dst
			for _, j := range jobs {
				if j.err == nil {
					j.r.saved = j.dst
				}
			}
			rs.err = ""
			if len(failed) > 0 {
				rs.err = fmt.Sprintf("%d 个文件保存失败：\n%s", len(failed), strings.Join(failed, "\n"))
			}
		})
	}()
}

// saveFileAs 打开保存对话框，把 src 复制到用户选择的位置；done 在 UI 线程调用（用户取消时不调用）。
// 默认文件名为 name 加 src 的扩展名，在上次保存的目录中去重，避免覆盖已有文件；
// 用户在对话框里选择已有文件时由系统对话框确认覆盖。
func (a *app) saveFileAs(src, name string, done func(dst string, err error)) {
	dir := a.saveDir
	if dir == "" {
		dir = a.be.layout.Outputs
	}
	ext := filepath.Ext(src)
	def := uniquePath(dir, safeFileName(name)+ext)
	win := a.win
	go func() {
		dst, err := mygo.Dialog.Save(mygo.SaveDialogOptions{
			Parent: win, Title: "保存", DefaultPath: def, CreateDirectories: true,
			Filters: []mygo.FileFilter{{Name: strings.ToUpper(strings.TrimPrefix(ext, ".")), Extensions: []string{strings.TrimPrefix(ext, ".")}}},
		})
		if err != nil || dst == "" {
			return
		}
		// 保存只复制原格式；用户改掉扩展名时补回，避免写出扩展名与内容不符的文件。
		if !strings.EqualFold(filepath.Ext(dst), ext) {
			dst += ext
		}
		err = copyFileAtomic(src, dst)
		a.post(func() {
			if err == nil {
				a.saveDir = filepath.Dir(dst)
			}
			done(dst, err)
		})
	}()
}

// uniquePath 返回 dir 下不与已有文件重名的路径：name 已存在时依次尝试「名称 (2).扩展名」……
func uniquePath(dir, name string) string {
	ext := filepath.Ext(name)
	base := strings.TrimSuffix(name, ext)
	p := filepath.Join(dir, name)
	for i := 2; ; i++ {
		if _, err := os.Stat(p); err != nil {
			return p
		}
		p = filepath.Join(dir, fmt.Sprintf("%s (%d)%s", base, i, ext))
	}
}

// safeFileName 把 Windows 与 macOS 文件名中不允许的字符替换为下划线。
func safeFileName(name string) string {
	name = strings.Map(func(r rune) rune {
		if strings.ContainsRune(`<>:"/\|?*`, r) || r < 32 {
			return '_'
		}
		return r
	}, strings.TrimSpace(name))
	if name == "" {
		return "output"
	}
	return name
}

// copyFileAtomic 先写到同目录的临时文件再改名，复制中途失败不会留下半个文件或破坏已有的同名文件。
func copyFileAtomic(src, dst string) error {
	in, err := os.Open(src)
	if err != nil {
		return fmt.Errorf("读取暂存文件：%w（暂存区可能已被清理）", err)
	}
	defer in.Close()
	tmp, err := os.CreateTemp(filepath.Dir(dst), ".kanade-save-*")
	if err != nil {
		return err
	}
	if _, err = io.Copy(tmp, in); err == nil {
		err = tmp.Close()
	} else {
		tmp.Close()
	}
	if err == nil {
		err = os.Rename(tmp.Name(), dst)
	}
	if err != nil {
		os.Remove(tmp.Name())
	}
	return err
}
