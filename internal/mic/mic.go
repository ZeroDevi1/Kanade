// Package mic 从系统默认输入设备录音：单声道 float32、固定采样率，供 K 歌跟唱时实时检测音高并保存演唱。
//
// mygo 的 audio 包只有输出，这里按同样的方式直接调用系统接口：macOS 用 Audio Queue（AudioToolbox，经 purego），
// Windows 用 winmm 的 waveIn。同一时间只支持一个录音。macOS 首次录音时系统会询问麦克风权限：打包的应用需要
// Info.plist 中的 NSMicrophoneUsageDescription（见 mygo.json），否则系统会直接结束应用；开发时以终端的权限为准。
package mic

import "errors"

// Rate 是录音的采样率（Hz）：32 kHz 足够保存可回放的演唱，降一半即为音高检测用的 16 kHz。
const Rate = 32000

// ErrUnsupported 表示当前平台没有实现录音。
var ErrUnsupported = errors.New("当前系统不支持录音")

// Capture 是一个进行中的录音。Stop 之后不再回调。
type Capture struct {
	stop func()
}

// Start 打开默认输入设备开始录音。onSamples 在录音线程上按到达顺序调用，每次约 32 ms 的采样；
// 切片在回调返回后会被复用，需要保留时须复制。设备打不开（没有麦克风、权限被拒绝）时返回错误。
func Start(onSamples func([]float32)) (*Capture, error) {
	stop, err := start(onSamples)
	if err != nil {
		return nil, err
	}
	return &Capture{stop: stop}, nil
}

// Stop 停止录音并释放设备，返回前保证不再调用 onSamples。可重复调用。
func (c *Capture) Stop() {
	if c.stop != nil {
		c.stop()
		c.stop = nil
	}
}

// bufferFrames 是每个系统缓冲的帧数（32 ms）；buffers 是轮转的缓冲个数。
const (
	bufferFrames = 1024
	buffers      = 4
)
