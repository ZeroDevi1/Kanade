package mic

import (
	"errors"
	"fmt"
	"sync"
	"sync/atomic"
	"syscall"
	"unsafe"
)

// Windows：winmm 的 waveIn（16 位 PCM，系统负责重采样到 Rate）。用 CALLBACK_EVENT：驱动填满一个缓冲时置位事件，
// 录音 goroutine 等待事件、按顺序取出已完成（WHDR_DONE）的缓冲并重新加入队列。比 WASAPI 捕获简单，
// 延迟（约 4 × 32 ms）对跟唱的音高显示足够，演唱与伴奏的对齐由分析时的延迟估计补偿。

type waveFormatEx struct {
	formatTag      uint16
	channels       uint16
	samplesPerSec  uint32
	avgBytesPerSec uint32
	blockAlign     uint16
	bitsPerSample  uint16
	size           uint16
}

type waveHdr struct {
	data          *int16
	bufferLength  uint32
	bytesRecorded uint32
	user          uintptr
	flags         uint32
	loops         uint32
	next          uintptr
	reserved      uintptr
}

const (
	waveFormatPCM  = 1
	waveMapper     = 0xFFFFFFFF
	callbackEvent  = 0x00050000
	whdrDone       = 0x00000001
	waitTimeoutMs  = 200
	waitObject0    = 0
	mmsysErrNoErr  = 0
	recordChannels = 1
)

var (
	winmm                 = syscall.NewLazyDLL("winmm.dll")
	kernel32              = syscall.NewLazyDLL("kernel32.dll")
	procWaveInOpen        = winmm.NewProc("waveInOpen")
	procWaveInPrepare     = winmm.NewProc("waveInPrepareHeader")
	procWaveInUnprepare   = winmm.NewProc("waveInUnprepareHeader")
	procWaveInAddBuffer   = winmm.NewProc("waveInAddBuffer")
	procWaveInStart       = winmm.NewProc("waveInStart")
	procWaveInReset       = winmm.NewProc("waveInReset")
	procWaveInClose       = winmm.NewProc("waveInClose")
	procCreateEventW      = kernel32.NewProc("CreateEventW")
	procWaitForSingleObj  = kernel32.NewProc("WaitForSingleObject")
	procCloseHandle       = kernel32.NewProc("CloseHandle")
	active                atomic.Bool
	errAlreadyRecording   = errors.New("已有进行中的录音")
	errNoInputDeviceFound = errors.New("打开麦克风失败：没有可用的录音设备，或在「设置 → 隐私和安全性 → 麦克风」中关闭了桌面应用的麦克风访问")
)

func start(fn func([]float32)) (func(), error) {
	if !active.CompareAndSwap(false, true) {
		return nil, errAlreadyRecording
	}
	event, _, _ := procCreateEventW.Call(0, 0, 0, 0)
	if event == 0 {
		active.Store(false)
		return nil, errors.New("打开麦克风失败：CreateEvent")
	}
	f := waveFormatEx{formatTag: waveFormatPCM, channels: recordChannels, samplesPerSec: Rate,
		avgBytesPerSec: Rate * 2, blockAlign: 2, bitsPerSample: 16}
	var h uintptr
	if r, _, _ := procWaveInOpen.Call(uintptr(unsafe.Pointer(&h)), waveMapper, uintptr(unsafe.Pointer(&f)), event, 0, callbackEvent); r != mmsysErrNoErr {
		procCloseHandle.Call(event)
		active.Store(false)
		if r == 2 { // MMSYSERR_BADDEVICEID：没有输入设备
			return nil, errNoInputDeviceFound
		}
		return nil, fmt.Errorf("打开麦克风失败（waveInOpen %d）", r)
	}
	// 缓冲与头放在 Go 堆上并由本函数的闭包持有到 Stop 结束，驱动在此期间写入。
	data := make([][]int16, buffers)
	hdrs := make([]waveHdr, buffers)
	for i := range hdrs {
		data[i] = make([]int16, bufferFrames)
		hdrs[i] = waveHdr{data: &data[i][0], bufferLength: bufferFrames * 2}
		procWaveInPrepare.Call(h, uintptr(unsafe.Pointer(&hdrs[i])), unsafe.Sizeof(hdrs[i]))
		procWaveInAddBuffer.Call(h, uintptr(unsafe.Pointer(&hdrs[i])), unsafe.Sizeof(hdrs[i]))
	}
	if r, _, _ := procWaveInStart.Call(h); r != mmsysErrNoErr {
		procWaveInReset.Call(h)
		for i := range hdrs {
			procWaveInUnprepare.Call(h, uintptr(unsafe.Pointer(&hdrs[i])), unsafe.Sizeof(hdrs[i]))
		}
		procWaveInClose.Call(h)
		procCloseHandle.Call(event)
		active.Store(false)
		return nil, fmt.Errorf("无法开始录音（waveInStart %d）", r)
	}

	var stopping atomic.Bool
	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		out := make([]float32, bufferFrames)
		next := 0 // 驱动按加入顺序填充缓冲，从 next 起依次检查
		for !stopping.Load() {
			procWaitForSingleObj.Call(event, waitTimeoutMs)
			for !stopping.Load() && hdrs[next].flags&whdrDone != 0 {
				hd := &hdrs[next]
				n := int(hd.bytesRecorded / 2)
				for i := range n {
					out[i] = float32(data[next][i]) / 32768
				}
				if n > 0 {
					fn(out[:n])
				}
				hd.flags &^= whdrDone
				hd.bytesRecorded = 0
				procWaveInAddBuffer.Call(h, uintptr(unsafe.Pointer(hd)), unsafe.Sizeof(*hd))
				next = (next + 1) % buffers
			}
		}
	}()
	return func() {
		stopping.Store(true)
		wg.Wait() // 录音 goroutine 退出后才释放缓冲，并保证不再调用 fn
		procWaveInReset.Call(h)
		for i := range hdrs {
			procWaveInUnprepare.Call(h, uintptr(unsafe.Pointer(&hdrs[i])), unsafe.Sizeof(hdrs[i]))
		}
		procWaveInClose.Call(h)
		procCloseHandle.Call(event)
		active.Store(false)
	}, nil
}
