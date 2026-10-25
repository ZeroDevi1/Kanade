package score

import (
	"cmp"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"slices"
)

// 改乐器：多乐器转录把音色相近的乐器认错时（例如钢琴被认成吉他），在结果上直接把一条乐器音轨改成另一个乐器组，
// 不必带着乐器提示重新转录（转录是分析中最慢的一步）。改成已有的乐器时两条音轨合并。结果写回 analysis.json，
// 之后的导出（JSON、MIDI）与重新打开的记录都用改过的乐器。

// RelabelInstrument 把转录中的乐器 from 改为乐器组 to（InstrumentGroups 的 Key）：to 已存在时合并音符，否则改名并
// 换成该组的代表音色。鼓与有音高乐器之间不能互改（鼓的音高是打击乐键位）。没有转录结果、乐器不存在或写文件失败时
// 返回错误，此时结果不变。
func (r *Run) RelabelInstrument(from, to string) error {
	var group *InstrumentGroup
	for i := range InstrumentGroups {
		if InstrumentGroups[i].Key == to {
			group = &InstrumentGroups[i]
		}
	}
	if group == nil {
		return fmt.Errorf("未知的乐器组 %q", to)
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	res := r.result
	if res == nil || res.Transcription == nil {
		return errors.New("没有多乐器转录结果")
	}
	ins := slices.Clone(res.Transcription.Instruments)
	k := slices.IndexFunc(ins, func(in Instrument) bool { return in.Name == from })
	if k < 0 {
		return fmt.Errorf("转录中没有乐器 %q", from)
	}
	if from == to {
		return nil
	}
	if ins[k].Drum != (to == "drums") {
		return errors.New("鼓与有音高的乐器不能互改")
	}
	if j := slices.IndexFunc(ins, func(in Instrument) bool { return in.Name == to }); j >= 0 {
		merged := append(slices.Clone(ins[j].Notes), ins[k].Notes...)
		slices.SortStableFunc(merged, func(x, y InstrumentNote) int {
			return cmp.Or(cmp.Compare(x.Start, y.Start), cmp.Compare(x.Pitch, y.Pitch))
		})
		ins[j].Notes = merged
		ins = slices.Delete(ins, k, k+1)
	} else {
		ins[k] = Instrument{Name: to, Program: group.Program, Drum: ins[k].Drum, Notes: ins[k].Notes}
	}
	// 与引擎一致：按音符数从多到少排列。
	slices.SortStableFunc(ins, func(x, y Instrument) int { return cmp.Compare(len(y.Notes), len(x.Notes)) })

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
