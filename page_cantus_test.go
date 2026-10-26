package main

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/egoist/mygo/ui"

	"kanade/engines"
	"kanade/internal/cantus"
	"kanade/internal/conductor"
	"kanade/internal/paths"
)

// 界面输入经 cantus.Request 组装后提交给 Conductor（不启动调度，任务停在队列中）。
func TestCantusSubmitsZeroShot(t *testing.T) {
	l := paths.New(t.TempDir())
	m := cantus.Models[0]
	os.MkdirAll(m.Dir(l), 0o755)
	os.WriteFile(filepath.Join(m.Dir(l), ".kanade-complete"), []byte("{}"), 0o644)
	cond, err := conductor.New(l, engines.FS, nil)
	if err != nil {
		t.Fatal(err)
	}

	a := newApp()
	a.be = backend{layout: l, cond: cond}
	a.current = "cantus"
	a.cantus.promptWav, a.cantus.promptText, a.cantus.text = "/tmp/p.wav", "参考", "目标文本"
	tt := ui.NewTester(a.view, 1200, 1400)
	if err := tt.Click("生成"); err != nil {
		t.Fatal(err)
	}
	jobs := cond.Jobs()
	if len(jobs) != 1 {
		t.Fatalf("jobs %+v, err %q", jobs, a.cantus.err)
	}
	j := jobs[0]
	if j.Task != "cosyvoice.zero_shot" || j.Params["prompt_wav"] != "/tmp/p.wav" || j.Params["text"] != "目标文本" || j.Params["model_dir"] != m.Dir(l) {
		t.Errorf("job %+v", j)
	}

	// 智能模式：清空参考文本后改用跨语种复刻（不需要参考文本）。
	cond.Cancel(j.ID)
	a.cantus.promptText = ""
	tt.Frame()
	if err := tt.Click("生成"); err != nil {
		t.Fatal(err)
	}
	if jobs := cond.Jobs(); len(jobs) != 2 || jobs[1].Task != "cosyvoice.cross_lingual" || jobs[1].Params["seed"] != nil {
		t.Fatalf("smart without prompt text: %+v", jobs)
	}

	// 专业模式：零样本克隆缺少参考文本时不提交，并在界面显示原因；固定种子随任务提交。
	cond.Cancel(cond.Jobs()[1].ID)
	a.cantus.synthMode, a.cantus.seedFixed = modePro, true
	tt.Frame()
	if err := tt.Click("生成"); err != nil {
		t.Fatal(err)
	}
	tt.Frame()
	if len(cond.Jobs()) != 2 || !tt.HasText("零样本克隆需要填写参考音频对应的文本") {
		t.Errorf("jobs %d, texts %q", len(cond.Jobs()), tt.Texts())
	}
	a.cantus.promptText = "参考"
	tt.Frame()
	if err := tt.Click("生成"); err != nil {
		t.Fatal(err)
	}
	if jobs := cond.Jobs(); len(jobs) != 3 || jobs[2].Task != "cosyvoice.zero_shot" || jobs[2].Params["seed"] != 1986 {
		t.Errorf("pro job %+v", jobs[len(jobs)-1])
	}
}

// 长文本：结果带 segments.json 时列出各段（时长异常的段给出提示），可单独重新生成一段：用合成时的请求、带上
// regenerate 参数提交。文本框下方显示字数与预计时长。
func TestCantusSegments(t *testing.T) {
	l := paths.New(t.TempDir())
	m := cantus.Models[0]
	os.MkdirAll(m.Dir(l), 0o755)
	os.WriteFile(filepath.Join(m.Dir(l), ".kanade-complete"), []byte("{}"), 0o644)
	cond, _ := conductor.New(l, engines.FS, nil)
	a := newApp()
	a.be, a.current = backend{layout: l, cond: cond}, "cantus"
	a.post = func(fn func()) {}

	dir := t.TempDir()
	audio := filepath.Join(dir, "j1.wav")
	writeTestWav(t, audio, 22050, 22050)
	for _, f := range []string{"seg_000.wav", "seg_001.wav", "seg_002.wav"} {
		writeTestWav(t, filepath.Join(dir, f), 22050, 2205)
	}
	manifest := filepath.Join(dir, "segments.json")
	os.WriteFile(manifest, []byte(`{"version":1,"sample_rate":22050,"partial":false,"audio":"j1.wav","segments":[
		{"text":"第一句。","pause":"sentence","file":"seg_000.wav","duration":1.2},
		{"text":"第二句很长，被模型拖长了。","pause":"paragraph","file":"seg_001.wav","duration":9.5,"retries":2,"suspicious":true},
		{"text":"第三句。","pause":"end","file":"seg_002.wav","duration":1.1}]}`), 0o644)
	req := cantus.Request{Model: m, Mode: cantus.ModeCrossLingual, Text: "第一句。第二句很长，被模型拖长了。\n\n第三句。",
		PromptWav: "/tmp/p.wav", Speed: 1, TextFrontend: true}
	a.registerCantusOutput(conductor.Job{Artifacts: []string{audio, manifest}}, "测试", "测试", req)

	a.cantus.text, a.cantus.promptWav = "今天天气很好。\n明天下雨。", "/tmp/p.wav"
	tt := ui.NewTester(a.view, 1200, 2400)
	if !tt.HasText("12 字 · 约 2 句 · 预计 0:02.2 语音") {
		t.Errorf("text estimate: %q", tt.Texts())
	}
	// 文本框中按 Cmd+Enter 提交合成（随后取消，不影响下面重新生成的检查）。
	if err := tt.Click("要合成的文本"); err != nil {
		t.Fatal(err)
	}
	tt.Key(ui.Cmd, ui.KeyEnter)
	if jobs := cond.Jobs(); len(jobs) != 1 || jobs[0].Task != "cosyvoice.cross_lingual" {
		t.Fatalf("Cmd+Enter: jobs %+v err %q", jobs, a.cantus.err)
	}
	cond.Cancel(cond.Jobs()[0].ID)
	tt.Frame() // 取消后进度条消失、布局上移，按新布局查找按钮
	if !tt.HasText("1 段时长异常（可能读错、拖长或漏读），建议试听后重新生成") {
		t.Fatalf("segments summary: %q", tt.Texts())
	}
	if err := tt.Click("分段（3 段）"); err != nil {
		t.Fatal(err)
	}
	if !tt.HasText("第二句很长，被模型拖长了。") || !tt.HasText("9.5 秒") {
		t.Fatalf("segment rows: %q", tt.Texts())
	}
	a.regenerateSegment(audio, manifest, 1)
	jobs := cond.Jobs()
	if len(jobs) != 2 || jobs[1].Task != "cosyvoice.cross_lingual" {
		t.Fatalf("jobs %+v err %q", jobs, a.cantus.err)
	}
	g, _ := jobs[1].Params["regenerate"].(map[string]any)
	if g["manifest"] != manifest || g["index"] != 1 || jobs[1].Params["text"] != req.Text {
		t.Errorf("regenerate params %+v", jobs[1].Params)
	}
	tt.Frame()
	if !tt.HasText("生成中…") {
		t.Errorf("regenerating row: %q", tt.Texts())
	}
}

// 其他引擎：选择 Qwen3-TTS 音色设计模型时，智能模式只需音色描述与文本，任务提交给 qwentts 引擎；
// 专业模式不显示该模型没有的语速与采样参数。
func TestCantusVoiceDesign(t *testing.T) {
	l := paths.New(t.TempDir())
	var m cantus.Model
	for _, x := range cantus.Models {
		if x.Family == cantus.FamilyQwenDesign {
			m = x
			break
		}
	}
	os.MkdirAll(m.Dir(l), 0o755)
	os.WriteFile(filepath.Join(m.Dir(l), ".kanade-complete"), []byte("{}"), 0o644)
	cond, err := conductor.New(l, engines.FS, nil)
	if err != nil {
		t.Fatal(err)
	}
	a := newApp()
	a.be, a.current = backend{layout: l, cond: cond}, "cantus"
	a.cantus.modelTitle, a.cantus.text = m.Title, "目标文本"
	tt := ui.NewTester(a.view, 1200, 1400)
	if !tt.HasText("音色描述") || tt.HasText("音色来源") {
		t.Fatalf("design input: %q", tt.Texts())
	}
	if err := tt.Click("生成"); err != nil {
		t.Fatal(err)
	}
	tt.Frame()
	if len(cond.Jobs()) != 0 || !tt.HasText("请用文字描述想要的音色") {
		t.Fatalf("empty description: jobs %d texts %q", len(cond.Jobs()), tt.Texts())
	}
	a.cantus.instruct = "年轻女性，声音温柔"
	tt.Frame()
	if err := tt.Click("生成"); err != nil {
		t.Fatal(err)
	}
	jobs := cond.Jobs()
	if len(jobs) != 1 || jobs[0].Engine != "qwentts" || jobs[0].Task != "qwentts.voice_design" || jobs[0].Params["instruct"] != "年轻女性，声音温柔" {
		t.Fatalf("jobs %+v err %q", jobs, a.cantus.err)
	}
	if opts, _ := jobs[0].Params["options"].(map[string]any); opts["temperature"] != 0.9 || opts["top_k"] != 50 {
		t.Errorf("smart options %v", jobs[0].Params["options"])
	}
	cond.Cancel(jobs[0].ID)
	a.cantus.synthMode = modePro
	tt.Frame()
	if tt.HasText("语速") || tt.HasText("自定义采样参数") || !tt.HasText("固定随机种子") || !tt.HasText("重复惩罚") {
		t.Errorf("pro tuning: %q", tt.Texts())
	}
	// 专业模式修改的参数随任务提交；「恢复推荐值」改回推荐值。
	*a.cantus.optNum["qwentts.temperature"] = 0.5
	if err := tt.Click("生成"); err != nil {
		t.Fatal(err)
	}
	jobs = cond.Jobs()
	if opts, _ := jobs[len(jobs)-1].Params["options"].(map[string]any); len(jobs) != 2 || opts["temperature"] != 0.5 {
		t.Fatalf("pro options: jobs %d %v err %q", len(jobs), jobs[len(jobs)-1].Params["options"], a.cantus.err)
	}
	cond.Cancel(jobs[1].ID)
	tt.Frame()
	if err := tt.Click("恢复推荐值"); err != nil {
		t.Fatal(err)
	}
	if v := *a.cantus.optNum["qwentts.temperature"]; v != 0.9 {
		t.Errorf("reset temperature %v", v)
	}
}
