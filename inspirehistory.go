package main

import (
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"time"

	"github.com/egoist/mygo/ui"

	"kanade/internal/mic"
	"kanade/internal/score"
)

// 灵感记录：每次录音整理后在 outputs/inspire-history/<时间>/ 下保存 record.json（节拍设置、录音帧、编辑后的音符、
// 和声与发展的选择）与 recording.wav（原始录音）；之后的编辑、和声与发展的选择在下一帧写回 record.json。
// 录音帧一并保存，既能重新整理（调节奏），也留作以后调整整理算法的真实数据。页面上可打开或删除以前的记录。

// inspireHistoryDir 是灵感记录在输出目录下的子目录名。
const inspireHistoryDir = "inspire-history"

// inspireRecord 是 record.json 的内容。
type inspireRecord struct {
	Created   time.Time            `json:"created"`
	RecSet    score.HumSettings    `json:"rec_settings"`
	TempoHint float64              `json:"tempo_hint,omitempty"`
	MeterPick int                  `json:"meter_pick,omitempty"`
	BarShift  int                  `json:"bar_shift,omitempty"`
	Frames    []score.KaraokeFrame `json:"frames"`
	Hum       score.Hum            `json:"hum"`
	// Raw 是整理出的原始音符（精简之前），Simplify 为精简程度；旧记录没有这两项（未精简）。
	Raw      []score.MelodyNote `json:"raw,omitempty"`
	Simplify int                `json:"simplify,omitempty"`
	Filler   int                `json:"filler"`
	Style    int                `json:"style"`
	Pick     int                `json:"pick"`
	Locks    []score.ChordLock  `json:"locks,omitempty"`
	DevBars  int                `json:"dev_bars"`
	DevForm  string             `json:"dev_form,omitempty"`
}

// inspireHistory 是记录列表的状态（属于 inspireState）。dir 是当前记录的目录（空为还没保存：新录音整理后的
// 第一次保存时创建）；items 为已有记录（最新在前），loaded 为 false 时进入页面会读一次。
type inspireHistory struct {
	dir           string
	items         []inspireHistoryItem
	loaded        bool
	sel           string // 下拉框的当前选择（标题）
	confirmDelete bool
}

type inspireHistoryItem struct {
	dir, title string
}

// historyRoot 返回记录的根目录；后端未启动（没有输出目录）时为空。
func (a *app) historyRoot() string {
	if a.be.layout.Outputs == "" {
		return ""
	}
	return filepath.Join(a.be.layout.Outputs, inspireHistoryDir)
}

// recordTitle 是记录在列表里的标题：时间 · 小节数 · 调 · 音数。
func recordTitle(r inspireRecord) string {
	h := r.Hum
	key := score.KeyLabel(h.TonicPC, h.Mode, score.KeySignature(h.TonicPC, h.Mode) < 0)
	return fmt.Sprintf("%s · %d 小节 · %s · %d 个音", r.Created.Format("01-02 15:04"), h.Settings.Bars, key, len(h.Notes))
}

// loadHistory 读入全部记录的标题（最新在前）；读不了的记录跳过。
func (a *app) loadHistory() {
	hs := &a.inspire.history
	hs.loaded, hs.items = true, nil
	root := a.historyRoot()
	if root == "" {
		return
	}
	entries, _ := os.ReadDir(root)
	for _, e := range entries {
		if !e.IsDir() {
			continue
		}
		dir := filepath.Join(root, e.Name())
		r, err := readInspireRecord(dir)
		if err != nil {
			continue
		}
		hs.items = append(hs.items, inspireHistoryItem{dir, recordTitle(r)})
	}
	slices.Reverse(hs.items) // 目录名按时间命名，倒序即最新在前
}

func readInspireRecord(dir string) (inspireRecord, error) {
	var r inspireRecord
	b, err := os.ReadFile(filepath.Join(dir, "record.json"))
	if err != nil {
		return r, err
	}
	return r, json.Unmarshal(b, &r)
}

// saveInspireRecord 把当前记录写回磁盘（新录音的第一次保存时创建目录并写入录音）。没有整理结果、没有输出目录时
// 不保存；写入失败显示在页面上，下一次改动时再试。
func (a *app) saveInspireRecord() {
	s := &a.inspire
	s.dirty = false
	root := a.historyRoot()
	if s.hum == nil || len(s.hum.Notes) == 0 || root == "" {
		return
	}
	hs := &s.history
	created := time.Now()
	if hs.dir == "" {
		hs.dir = filepath.Join(root, created.Format("20060102-150405.000"))
		if err := os.MkdirAll(hs.dir, 0o755); err != nil {
			s.err, hs.dir = "保存灵感记录失败："+err.Error(), ""
			return
		}
		if err := os.WriteFile(filepath.Join(hs.dir, "recording.wav"), wav16(s.recording, mic.Rate), 0o644); err != nil {
			s.err = "保存录音失败：" + err.Error()
		}
	} else if old, err := readInspireRecord(hs.dir); err == nil {
		created = old.Created
	}
	r := inspireRecord{Created: created, RecSet: s.recSet, TempoHint: s.tempoHint, MeterPick: s.meterPick, BarShift: s.barShift,
		Frames: s.frames, Hum: *s.hum, Raw: s.raw, Simplify: s.simplify, Filler: s.filler, Style: s.style, Pick: s.pick, Locks: s.locks,
		DevBars: s.dev.bars, DevForm: s.dev.form}
	b, err := json.Marshal(r)
	if err == nil {
		err = os.WriteFile(filepath.Join(hs.dir, "record.json"), b, 0o644)
	}
	if err != nil {
		s.err = "保存灵感记录失败：" + err.Error()
		return
	}
	// 更新列表里这一项的标题（新记录插到最前）。
	title := recordTitle(r)
	if i := slices.IndexFunc(hs.items, func(it inspireHistoryItem) bool { return it.dir == hs.dir }); i >= 0 {
		hs.items[i].title = title
	} else {
		hs.items = slices.Insert(hs.items, 0, inspireHistoryItem{hs.dir, title})
	}
	hs.sel = title
}

// openInspireRecord 打开一条记录：恢复录音、帧、音符与各项选择（撤销记录清空）。
func (a *app) openInspireRecord(dir string) {
	s := &a.inspire
	r, err := readInspireRecord(dir)
	if err != nil {
		s.err = "打开灵感记录失败：" + err.Error()
		return
	}
	rec, err := readWav16(filepath.Join(dir, "recording.wav"))
	if err != nil {
		s.err = "读取录音失败：" + err.Error()
	}
	a.pauseAllPlayers()
	h := r.Hum
	s.recSet, s.tempoHint, s.meterPick, s.barShift = r.RecSet, r.TempoHint, r.MeterPick, r.BarShift
	s.frames, s.recording, s.hum = r.Frames, rec, &h
	s.raw, s.simplify = r.Raw, r.Simplify
	if s.raw == nil {
		s.raw = slices.Clone(h.Notes) // 旧记录：保存的音符就是原始结果
	}
	s.filler, s.style, s.locks = r.Filler, r.Style, r.Locks
	s.sel, s.undo, s.redo, s.history.dir = -1, nil, nil, dir
	s.dev.bars, s.dev.form = r.DevBars, r.DevForm
	s.free = r.RecSet.Free
	s.changed()
	if r.Pick < len(s.harmonies) {
		s.pick = r.Pick
	}
	s.dirty = false // 刚读入，与磁盘一致
}

// deleteInspireRecord 删除当前记录（目录整个删掉）并清空页面。
func (a *app) deleteInspireRecord() {
	s := &a.inspire
	hs := &s.history
	if hs.dir == "" {
		return
	}
	if err := os.RemoveAll(hs.dir); err != nil {
		s.err = "删除灵感记录失败：" + err.Error()
		return
	}
	hs.items = slices.DeleteFunc(hs.items, func(it inspireHistoryItem) bool { return it.dir == hs.dir })
	if s.player != nil {
		s.player.Stop()
	}
	s.dev.reset()
	*s = inspireState{free: s.free, bpm: s.bpm, meter: s.meter, bars: s.bars, sel: -1, chordSel: -1, chords: s.chords, simplify: s.simplify,
		playerVersion: 1, dev: s.dev, history: inspireHistory{items: hs.items, loaded: true}}
}

// inspireHistoryRow 是记录列表：选择即打开；删除需要再点一次确认。
func (a *app) inspireHistoryRow(c *ui.Context) {
	s := &a.inspire
	hs := &s.history
	if !hs.loaded {
		a.loadHistory()
	}
	if len(hs.items) == 0 {
		return
	}
	titles := make([]string, len(hs.items))
	for i, it := range hs.items {
		titles[i] = it.title
		if it.dir == hs.dir {
			hs.sel = it.title
		}
	}
	if !slices.Contains(titles, hs.sel) {
		hs.sel = ""
	}
	ui.Row(c).Wrap().Gap(8).AlignItems(ui.Center).Children(func() {
		ui.Text(c, "灵感记录").FontSize(12)
		if ui.Select(c, &hs.sel, titles).Label("灵感记录").Width(300).Disabled(s.recordingNow()).Changed() {
			hs.confirmDelete = false
			if i := slices.Index(titles, hs.sel); i >= 0 {
				a.openInspireRecord(hs.items[i].dir)
			}
		}
		if hs.dir == "" {
			return
		}
		label := "删除记录"
		if hs.confirmDelete {
			label = "确认删除？"
		}
		if ui.Button(c, label).Disabled(s.recordingNow()).Clicked() {
			if hs.confirmDelete {
				a.deleteInspireRecord()
			} else {
				hs.confirmDelete = true
			}
		}
	})
}

// readWav16 读取 wav16 写出的 16 位单声道 PCM WAV（按 RIFF 块查找 data 块）。
func readWav16(path string) ([]float32, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	if len(b) < 12 || string(b[:4]) != "RIFF" || string(b[8:12]) != "WAVE" {
		return nil, errors.New("不是 WAV 文件")
	}
	for i := 12; i+8 <= len(b); {
		id, size := string(b[i:i+4]), int(binary.LittleEndian.Uint32(b[i+4:]))
		if id == "data" {
			data := b[i+8 : min(len(b), i+8+size)]
			out := make([]float32, len(data)/2)
			for j := range out {
				out[j] = float32(int16(binary.LittleEndian.Uint16(data[2*j:]))) / 32767
			}
			return out, nil
		}
		i += 8 + size + size%2
	}
	return nil, errors.New("WAV 中没有数据块")
}
