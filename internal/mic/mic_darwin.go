package mic

import (
	"errors"
	"fmt"
	"sync"
	"sync/atomic"
	"unsafe"

	"github.com/ebitengine/purego"
)

// macOS：Audio Queue 输入（AudioQueueNewInput），回调在队列自己的线程上运行（不传 RunLoop）。
// 结构体布局与 mygo/audio 的输出实现相同（AudioToolbox 的 C 定义）。

type streamDesc struct {
	sampleRate       float64
	formatID         uint32
	formatFlags      uint32
	bytesPerPacket   uint32
	framesPerPacket  uint32
	bytesPerFrame    uint32
	channelsPerFrame uint32
	bitsPerChannel   uint32
	reserved         uint32
}

type queueBuffer struct {
	capacity           uint32
	data               unsafe.Pointer
	byteSize           uint32
	userData           uintptr
	packetDescCapacity uint32
	packetDescs        uintptr
	packetDescCount    uint32
}

const (
	formatLinearPCM = 0x6C70636D // 'lpcm'
	formatIsFloat   = 1
	formatIsPacked  = 8
)

var (
	loadOnce sync.Once
	loadErr  error

	newInput       func(format *streamDesc, callback, user, runLoop, mode uintptr, flags uint32, out *uintptr) int32
	allocateBuffer func(q uintptr, size uint32, out **queueBuffer) int32
	enqueueBuffer  func(q uintptr, b *queueBuffer, n uint32, descs uintptr) int32
	queueStart     func(q uintptr, at uintptr) int32
	queueStop      func(q uintptr, immediate bool) int32
	queueDispose   func(q uintptr, immediate bool) int32

	// callback 只创建一次（purego 的回调数量有上限），经 current 找到当前录音。
	callback uintptr
	current  atomic.Pointer[darwinCapture]
)

type darwinCapture struct {
	q       uintptr
	fn      func([]float32)
	stopped atomic.Bool
	mu      sync.Mutex // 回调持有期间 Stop 等待，保证 Stop 返回后不再调用 fn
}

func load() error {
	loadOnce.Do(func() {
		lib, err := purego.Dlopen("/System/Library/Frameworks/AudioToolbox.framework/AudioToolbox", purego.RTLD_LAZY|purego.RTLD_GLOBAL)
		if err != nil {
			loadErr = err
			return
		}
		purego.RegisterLibFunc(&newInput, lib, "AudioQueueNewInput")
		purego.RegisterLibFunc(&allocateBuffer, lib, "AudioQueueAllocateBuffer")
		purego.RegisterLibFunc(&enqueueBuffer, lib, "AudioQueueEnqueueBuffer")
		purego.RegisterLibFunc(&queueStart, lib, "AudioQueueStart")
		purego.RegisterLibFunc(&queueStop, lib, "AudioQueueStop")
		purego.RegisterLibFunc(&queueDispose, lib, "AudioQueueDispose")
		// AudioQueueInputCallback(user, queue, buffer, startTime, numPackets, packetDescs)：只用到前三个参数，
		// 帧数从 buffer 的 byteSize 换算（numPackets 是 32 位参数，按 uintptr 接收时高位不可靠）。
		callback = purego.NewCallback(func(user, q uintptr, b *queueBuffer, start, n, descs uintptr) {
			d := current.Load()
			if d == nil || d.q != q {
				return
			}
			d.mu.Lock()
			defer d.mu.Unlock()
			if d.stopped.Load() {
				return
			}
			if k := int(b.byteSize / 4); k > 0 {
				d.fn(unsafe.Slice((*float32)(b.data), k))
			}
			enqueueBuffer(q, b, 0, 0)
		})
	})
	return loadErr
}

// devices 返回 nil：macOS 只录系统默认输入设备（Audio Queue 指定设备需另经 Core Audio 枚举设备 UID，尚未实现），
// 界面因此不提供选择。
func devices() []string { return nil }

// start 忽略 device，总是使用系统默认输入设备（见 devices）。
func start(_ string, fn func([]float32)) (func(), error) {
	if err := load(); err != nil {
		return nil, err
	}
	if current.Load() != nil {
		return nil, errors.New("已有进行中的录音")
	}
	desc := streamDesc{
		sampleRate: Rate, formatID: formatLinearPCM, formatFlags: formatIsFloat | formatIsPacked,
		bytesPerPacket: 4, framesPerPacket: 1, bytesPerFrame: 4, channelsPerFrame: 1, bitsPerChannel: 32,
	}
	d := &darwinCapture{fn: fn}
	if st := newInput(&desc, callback, 0, 0, 0, 0, &d.q); st != 0 {
		return nil, fmt.Errorf("打开麦克风失败（AudioQueueNewInput %d）", st)
	}
	for range buffers {
		var b *queueBuffer
		if st := allocateBuffer(d.q, bufferFrames*4, &b); st != 0 {
			queueDispose(d.q, true)
			return nil, fmt.Errorf("打开麦克风失败（AudioQueueAllocateBuffer %d）", st)
		}
		enqueueBuffer(d.q, b, 0, 0)
	}
	current.Store(d)
	if st := queueStart(d.q, 0); st != 0 {
		current.Store(nil)
		queueDispose(d.q, true)
		// 没有输入设备或用户拒绝了麦克风权限时在这里失败。
		return nil, fmt.Errorf("无法开始录音（AudioQueueStart %d）：请检查麦克风是否连接，并在「系统设置 → 隐私与安全性 → 麦克风」中允许 Kanade", st)
	}
	return func() {
		d.mu.Lock()
		d.stopped.Store(true)
		d.mu.Unlock()
		queueStop(d.q, true)
		queueDispose(d.q, true)
		current.CompareAndSwap(d, nil)
	}, nil
}
