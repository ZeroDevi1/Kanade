package score

import (
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"os"
	"reflect"
	"slices"
)

// 在结果上编辑转录（多轨卷帘）：
//   - 改乐器：多乐器转录把音色相近的乐器认错时（例如钢琴被认成吉他），把一条乐器音轨改成另一个乐器组，不必带着
//     乐器提示重新转录（转录是分析中最慢的一步）。改成已有的乐器时两条音轨仍各自独立（各有混音与演奏设置），
//     不合并音符。
//   - 演奏设置与逐音力度（Performance、InstrumentNote.Velocity）。
// 改动写回 analysis.json，之后的导出（JSON、MIDI）与重新打开的记录都用改过的结果。乐器按序号（Transcription.Instruments
// 的下标）指定：改乐器后可能有同名的两条音轨，名称不再唯一。编辑不改变乐器的顺序。

// RelabelInstrument 把转录中第 k 种乐器改为乐器组 to（InstrumentGroups 的 Key）并换成该组的代表音色，音符与演奏设置不变。
// 鼓与有音高乐器之间不能互改（鼓的音高是打击乐键位）。没有转录结果、序号越界或写文件失败时返回错误，此时结果不变。
func (r *Run) RelabelInstrument(k int, to string) error {
	var group *InstrumentGroup
	for i := range InstrumentGroups {
		if InstrumentGroups[i].Key == to {
			group = &InstrumentGroups[i]
		}
	}
	if group == nil {
		return fmt.Errorf("未知的乐器组 %q", to)
	}
	return r.editInstrument(k, func(in *Instrument) error {
		if in.Drum != (to == "drums") {
			return errors.New("鼓与有音高的乐器不能互改")
		}
		in.Name, in.Program = to, group.Program
		return nil
	})
}

// SetPerformance 设置第 k 种乐器的演奏设置。设置超出范围时返回错误，其余错误同 RelabelInstrument。
func (r *Run) SetPerformance(k int, p Performance) error {
	if !p.Valid() {
		return fmt.Errorf("演奏设置超出范围：%+v", p)
	}
	return r.editInstrument(k, func(in *Instrument) error {
		in.Play = p
		return nil
	})
}

// SetVelocities 设置第 k 种乐器的逐音力度：v 与该乐器的音符一一对应（0–1，0 为未设置）；v 为 nil 时全部清除。
// 长度不符或取值越界时返回错误，其余错误同 RelabelInstrument。
func (r *Run) SetVelocities(k int, v []float64) error {
	return r.editInstrument(k, func(in *Instrument) error {
		if v != nil && len(v) != len(in.Notes) {
			return fmt.Errorf("力度数 %d 与音符数 %d 不符", len(v), len(in.Notes))
		}
		notes := slices.Clone(in.Notes)
		for i := range notes {
			x := 0.0
			if v != nil {
				x = v[i]
			}
			if x < 0 || x > 1 || math.IsNaN(x) {
				return fmt.Errorf("力度 %v 超出 0–1", x)
			}
			notes[i].Velocity = x
		}
		in.Notes = notes
		return nil
	})
}

// editInstrument 对第 k 种乐器的副本执行 edit，成功且有变化时写回 analysis.json 并换上新结果。
// edit 只能改它拿到的 Instrument（切片字段须先复制再改）。
func (r *Run) editInstrument(k int, edit func(*Instrument) error) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	res := r.result
	if res == nil || res.Transcription == nil {
		return errors.New("没有多乐器转录结果")
	}
	ins := slices.Clone(res.Transcription.Instruments)
	if k < 0 || k >= len(ins) {
		return fmt.Errorf("转录中没有第 %d 种乐器", k)
	}
	if err := edit(&ins[k]); err != nil {
		return err
	}
	if reflect.DeepEqual(ins[k], res.Transcription.Instruments[k]) {
		return nil
	}

	// 新的结果是副本：界面以结果指针判断音轨是否需要重建，旧指针的读者看到的仍是改之前的完整结果。
	next := *res
	tr := *res.Transcription
	tr.Instruments = ins
	next.Transcription = &tr
	if path := r.files["analysis"]; path != "" {
		if err := rewriteTranscription(path, &tr); err != nil {
			return fmt.Errorf("保存改动：%w", err)
		}
	}
	r.result = &next
	return nil
}

// rewriteTranscription 只替换 analysis.json 中的 transcription 字段（其余字段原样保留），先写临时文件再重命名。
func rewriteTranscription(path string, tr *Transcription) error {
	data, err := os.ReadFile(path)
	if err != nil {
		return err
	}
	var doc map[string]json.RawMessage
	if err := json.Unmarshal(data, &doc); err != nil {
		return err
	}
	raw, err := json.Marshal(tr)
	if err != nil {
		return err
	}
	doc["transcription"] = raw
	out, err := json.MarshalIndent(doc, "", "  ")
	if err != nil {
		return err
	}
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, out, 0o644); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}
