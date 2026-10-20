package main

import (
	"bytes"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"time"

	"github.com/egoist/mygo"
	"github.com/egoist/mygo/audio"

	"kanade/engines"
	"kanade/internal/conductor"
	"kanade/internal/paths"
	"kanade/internal/settings"
	"kanade/internal/ui/waveform"
)

// backend 是启动时解析出的数据目录与 Conductor。startBackend 失败时 err 非空、cond 为 nil，界面显示错误。
type backend struct {
	userData string // 系统应用数据目录，存放 settings.json
	settings settings.Settings
	layout   paths.Layout
	cond     *conductor.Conductor
	err      error
}

// startBackend 读取设置、确定数据根目录并启动 Conductor。须在 mygo.App 就绪后调用（需要系统目录）。
// onChange 在 Conductor 状态变化时调用，可来自任意 goroutine。
func startBackend(onChange func()) backend {
	var b backend
	b.userData, b.err = mygo.App.Path(mygo.PathUserData)
	if b.err != nil {
		return b
	}
	if b.settings, b.err = settings.Load(b.userData); b.err != nil {
		b.err = fmt.Errorf("读取 %s: %w", filepath.Join(b.userData, settings.FileName), b.err)
		return b
	}
	root := b.settings.DataRoot
	if root == "" {
		docs, err := mygo.App.Path(mygo.PathDocuments)
		if err != nil {
			b.err = err
			return b
		}
		root = filepath.Join(docs, paths.DirName)
	}
	b.layout = paths.New(root)
	if b.err = b.layout.Ensure(); b.err != nil {
		return b
	}
	if b.cond, b.err = conductor.New(b.layout, engines.FS, onChange); b.err != nil {
		return b
	}
	b.cond.Start()
	return b
}

// loadedAudio 是解码后的音频：用于波形的峰值与播放器。
type loadedAudio struct {
	peaks  waveform.Peaks
	player *audio.Player
}

// loadAudio 读取 path 的音频文件（WAV，或 audio 包已注册的格式），解码为采样并计算峰值。
// 在后台 goroutine 调用：整文件读入内存，对数分钟的音频耗时约数百毫秒。
func loadAudio(path string) (loadedAudio, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return loadedAudio{}, err
	}
	dec, _, err := audio.NewDecoder(bytes.NewReader(data))
	if err != nil {
		return loadedAudio{}, fmt.Errorf("解码 %s: %w", filepath.Base(path), err)
	}
	ch := dec.Channels()
	var samples []float32
	buf := make([]float32, 4096*ch)
	for {
		n, err := dec.Read(buf)
		samples = append(samples, buf[:n]...)
		if err == io.EOF {
			break
		}
		if err != nil {
			return loadedAudio{}, err
		}
	}
	// NewSound 只接受单声道或立体声；更多声道时让 DecodeSound 自行取前两个声道。
	var snd *audio.Sound
	if ch <= 2 {
		snd = audio.NewSound(dec.SampleRate(), ch, samples)
	} else if snd, err = audio.DecodeSound(data); err != nil {
		return loadedAudio{}, err
	}
	return loadedAudio{
		peaks:  waveform.Compute(samples, ch, dec.SampleRate(), waveform.DefaultFramesPerBucket),
		player: snd.NewPlayer(),
	}, nil
}

// shutdownGrace 是退出时等待 worker 结束当前任务的上限；超时后强制结束进程。
const shutdownGrace = 5 * time.Second
