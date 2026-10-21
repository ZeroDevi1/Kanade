// Package conductor 管理 Python 推理后端：uv 引导、引擎环境同步、worker 进程与任务队列。
//
// 调度策略：一个 GPU 槽位，任务按提交顺序逐个执行（FIFO）。同一引擎的 worker 在任务间复用，
// 切换到其他引擎时关闭上一个 worker 以释放显存；之后可按显存预算扩展为多 worker 与 LRU 卸载。
package conductor

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"

	"kanade/internal/paths"
)

// JobState 是任务的生命周期状态：queued → preparing → running → done | failed | canceled。
type JobState string

const (
	JobQueued    JobState = "queued"
	JobPreparing JobState = "preparing" // 安装引擎环境或启动 worker
	JobRunning   JobState = "running"
	JobDone      JobState = "done"
	JobFailed    JobState = "failed"
	JobCanceled  JobState = "canceled"
)

// Finished 报告状态是否为终态。
func (s JobState) Finished() bool { return s == JobDone || s == JobFailed || s == JobCanceled }

// Job 是任务的快照，由 Jobs 复制返回，调用方可随意读取。
type Job struct {
	ID       string
	Engine   string
	Task     string
	Params   map[string]any
	State    JobState
	Progress float64 // [0, 1]
	Message  string
	// Artifacts 是 worker 登记的输出文件路径，按登记顺序。
	Artifacts []string
	Err       string
	Created   time.Time
	Ended     time.Time
}

// EngineState 是引擎环境的安装状态。
type EngineState string

const (
	EngineMissing    EngineState = "missing"    // 尚未安装或源码已更新，需要 uv sync
	EngineInstalling EngineState = "installing" // 正在释放源码或 uv sync
	EngineReady      EngineState = "ready"      // 环境就绪，worker 未运行
	EngineRunning    EngineState = "running"    // worker 运行中
	EngineFailed     EngineState = "failed"     // 最近一次安装或启动失败
)

// Engine 是引擎状态的快照。
type Engine struct {
	Spec  Spec
	State EngineState
	// Outdated 表示 State 为 EngineMissing 时虚拟环境已存在、只是源码已更新（例如应用升级后），
	// 安装只做增量 uv sync；界面据此显示「需要更新」而不是「未安装」。
	Outdated bool
	Err      string
	// Log 是最近的安装日志（最多 logLimit 行），用于界面显示；完整的 worker 日志在 Logs 目录。
	Log []string
}

const logLimit = 200

type engineEntry struct {
	spec     Spec
	state    EngineState
	outdated bool
	err      string
	log      []string
}

// Conductor 是推理后端的唯一入口。
//
// 所有导出方法可从任意 goroutine 调用；内部状态受 mu 保护。状态变化后调用 onChange（不持有锁），
// 界面据此重建。Start 之后才会执行任务；Shutdown 后不再接受新任务。
type Conductor struct {
	layout   paths.Layout
	src      fs.FS
	onChange func()

	mu      sync.Mutex
	uv      *UV
	engines map[string]*engineEntry
	order   []string // engines 的名称顺序，与 LoadSpecs 一致
	jobs    []*Job   // 提交顺序；终态任务保留在列表中供界面显示
	nextID  int
	cancels map[string]func() // 运行中或准备中任务的取消函数
	wake    chan struct{}     // 有新任务时唤醒 runner，容量 1
	worker  *Worker
	wEngine string // worker 所属引擎
	// extraEnv 是追加给 uv 与 worker 的环境变量（下载镜像），对之后启动的进程生效。
	extraEnv []string
	closed   bool
	ctx      context.Context
	stop     context.CancelFunc
	done     chan struct{}
}

// New 创建 Conductor：读取嵌入的引擎描述并检查各引擎是否已就绪。onChange 可为 nil。
func New(l paths.Layout, src fs.FS, onChange func()) (*Conductor, error) {
	specs, err := LoadSpecs(src)
	if err != nil {
		return nil, err
	}
	if onChange == nil {
		onChange = func() {}
	}
	ctx, stop := context.WithCancel(context.Background())
	c := &Conductor{
		layout: l, src: src, onChange: onChange,
		engines: map[string]*engineEntry{}, cancels: map[string]func(){},
		wake: make(chan struct{}, 1), ctx: ctx, stop: stop, done: make(chan struct{}),
	}
	for _, s := range specs {
		e := &engineEntry{spec: s, state: EngineMissing}
		if Ready(src, l, s.Name) {
			e.state = EngineReady
		} else if _, err := os.Stat(VenvPython(l, s.Name)); err == nil {
			e.outdated = true
		}
		c.engines[s.Name] = e
		c.order = append(c.order, s.Name)
	}
	c.uv = FindUV(l)
	return c, nil
}

// SetEnv 设置追加给 uv 与之后启动的 worker 的环境变量；已在运行的 worker 不受影响。
func (c *Conductor) SetEnv(env []string) {
	c.mu.Lock()
	c.extraEnv = append([]string(nil), env...)
	c.mu.Unlock()
}

func (c *Conductor) env() []string {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.extraEnv
}

// Start 启动任务调度 goroutine。只能调用一次。
func (c *Conductor) Start() { go c.run() }

// Layout 返回数据根目录布局。
func (c *Conductor) Layout() paths.Layout { return c.layout }

// UV 返回当前使用的 uv；尚未找到或下载时为 nil。
func (c *Conductor) UV() *UV {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.uv
}

// EnsureUV 返回可用的 uv，没有时下载。
func (c *Conductor) EnsureUV(ctx context.Context) (*UV, error) {
	if uv := c.UV(); uv != nil {
		return uv, nil
	}
	mirror := ""
	for _, kv := range c.env() {
		if v, ok := strings.CutPrefix(kv, "KANADE_GITHUB_MIRROR="); ok {
			mirror = v
		}
	}
	uv, err := DownloadUV(ctx, c.layout, mirror)
	if err != nil {
		return nil, err
	}
	c.mu.Lock()
	c.uv = uv
	c.mu.Unlock()
	c.onChange()
	return uv, nil
}

// Engines 返回所有引擎的状态快照。
func (c *Conductor) Engines() []Engine {
	c.mu.Lock()
	defer c.mu.Unlock()
	out := make([]Engine, 0, len(c.order))
	for _, name := range c.order {
		e := c.engines[name]
		out = append(out, Engine{Spec: e.spec, State: e.state, Outdated: e.outdated, Err: e.err, Log: append([]string(nil), e.log...)})
	}
	return out
}

// Jobs 返回所有任务的快照，按提交顺序。
func (c *Conductor) Jobs() []Job {
	c.mu.Lock()
	defer c.mu.Unlock()
	out := make([]Job, len(c.jobs))
	for i, j := range c.jobs {
		out[i] = *j
		out[i].Artifacts = append([]string(nil), j.Artifacts...)
	}
	return out
}

// Job 返回指定任务的快照；不存在时 ok 为 false。
func (c *Conductor) Job(id string) (j Job, ok bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	for _, x := range c.jobs {
		if x.ID == id {
			j = *x
			j.Artifacts = append([]string(nil), x.Artifacts...)
			return j, true
		}
	}
	return Job{}, false
}

// Submit 把任务加入队列并返回任务 ID。引擎不存在或不支持该任务时返回错误。
func (c *Conductor) Submit(engine, task string, params map[string]any) (string, error) {
	c.mu.Lock()
	e, ok := c.engines[engine]
	if !ok {
		c.mu.Unlock()
		return "", fmt.Errorf("未知引擎 %q", engine)
	}
	if !contains(e.spec.Tasks, task) {
		c.mu.Unlock()
		return "", fmt.Errorf("引擎 %s 不支持任务 %q", engine, task)
	}
	if c.closed {
		c.mu.Unlock()
		return "", errors.New("Conductor 已关闭")
	}
	c.nextID++
	id := time.Now().Format("20060102-150405") + "-" + strconv.Itoa(c.nextID)
	c.jobs = append(c.jobs, &Job{ID: id, Engine: engine, Task: task, Params: params, State: JobQueued, Created: time.Now()})
	c.mu.Unlock()
	c.signal()
	c.onChange()
	return id, nil
}

// Wait 等待任务进入终态并返回其快照。ctx 结束时返回 ctx.Err()，任务本身不受影响。
// 以 200 ms 间隔轮询：任务通常持续数秒到数小时，轮询开销可以忽略，也避免为每个任务维护订阅。
func (c *Conductor) Wait(ctx context.Context, id string) (Job, error) {
	for {
		j, ok := c.Job(id)
		if !ok {
			return Job{}, fmt.Errorf("任务 %s 不存在", id)
		}
		if j.State.Finished() {
			return j, nil
		}
		select {
		case <-ctx.Done():
			return j, ctx.Err()
		case <-time.After(200 * time.Millisecond):
		}
	}
}

// Cancel 取消任务：排队中的直接标记为已取消，运行中的通知 worker 在下一个检查点停止。
func (c *Conductor) Cancel(id string) {
	c.mu.Lock()
	var cancel func()
	for _, j := range c.jobs {
		if j.ID != id || j.State.Finished() {
			continue
		}
		if j.State == JobQueued {
			j.State, j.Ended = JobCanceled, time.Now()
		} else {
			cancel = c.cancels[id]
		}
	}
	c.mu.Unlock()
	if cancel != nil {
		cancel()
	}
	c.onChange()
}

// Install 在后台安装引擎环境（不提交任务），供设置页的「同步环境」按钮使用。
func (c *Conductor) Install(engine string) {
	go func() {
		_ = c.install(c.ctx, engine)
	}()
}

// Shutdown 停止接受任务，取消正在执行的任务并关闭 worker，最多等待 grace。
func (c *Conductor) Shutdown(grace time.Duration) {
	c.mu.Lock()
	if c.closed {
		c.mu.Unlock()
		return
	}
	c.closed = true
	c.mu.Unlock()
	c.stop()
	select {
	case <-c.done:
	case <-time.After(grace):
	}
	c.mu.Lock()
	w := c.worker
	c.worker = nil
	c.mu.Unlock()
	if w != nil {
		w.Close(grace)
	}
}

func (c *Conductor) signal() {
	select {
	case c.wake <- struct{}{}:
	default:
	}
}

// run 是调度循环：取出最早的排队任务执行，直到 Conductor 关闭。
func (c *Conductor) run() {
	defer close(c.done)
	for {
		job := c.nextQueued()
		if job == nil {
			select {
			case <-c.ctx.Done():
				return
			case <-c.wake:
				continue
			}
		}
		c.execute(job)
		if c.ctx.Err() != nil {
			return
		}
	}
}

func (c *Conductor) nextQueued() *Job {
	c.mu.Lock()
	defer c.mu.Unlock()
	for _, j := range c.jobs {
		if j.State == JobQueued {
			j.State = JobPreparing
			return j
		}
	}
	return nil
}

// update 在锁内修改任务并通知界面。
func (c *Conductor) update(j *Job, fn func(j *Job)) {
	c.mu.Lock()
	fn(j)
	c.mu.Unlock()
	c.onChange()
}

func (c *Conductor) fail(j *Job, err error) {
	c.update(j, func(j *Job) { j.State, j.Err, j.Ended = JobFailed, err.Error(), time.Now() })
}

// execute 执行一个任务：确保引擎就绪与 worker 运行，发送请求并消费事件直到终态。
func (c *Conductor) execute(j *Job) {
	ctx, cancel := context.WithCancel(c.ctx)
	defer cancel()
	c.mu.Lock()
	c.cancels[j.ID] = cancel
	c.mu.Unlock()
	defer func() {
		c.mu.Lock()
		delete(c.cancels, j.ID)
		c.mu.Unlock()
	}()

	// 准备阶段：安装环境、按需切换 worker。此阶段被取消时直接结束，不涉及 worker。
	c.update(j, func(j *Job) { j.Message = "准备引擎环境" })
	if err := c.install(ctx, j.Engine); err != nil {
		if ctx.Err() != nil {
			c.update(j, func(j *Job) { j.State, j.Ended = JobCanceled, time.Now() })
			return
		}
		c.fail(j, err)
		return
	}
	w, err := c.workerFor(j.Engine)
	if err != nil {
		c.fail(j, err)
		return
	}

	outDir := filepath.Join(c.layout.Scratch, j.Engine)
	c.update(j, func(j *Job) { j.State, j.Message = JobRunning, "已提交给 worker" })
	if err := w.Run(j.ID, j.Task, j.Params, outDir); err != nil {
		c.fail(j, fmt.Errorf("发送任务: %w", err))
		return
	}

	// 事件循环：ctx 取消时只通知 worker，仍等待其回报 canceled，保证 worker 空闲后再执行下一个任务。
	canceled := ctx.Done()
	for {
		select {
		case <-canceled:
			_ = w.Cancel(j.ID)
			canceled = nil
		case ev, ok := <-w.Events:
			if !ok {
				c.dropWorker(w)
				c.fail(j, fmt.Errorf("%w：%v，详见 %s.log", errWorkerExited, w.Err(), j.Engine))
				return
			}
			if ev.Job != j.ID {
				continue
			}
			if c.apply(j, ev) {
				return
			}
		}
	}
}

// apply 把一条事件应用到任务，返回任务是否已进入终态。
func (c *Conductor) apply(j *Job, ev Event) bool {
	switch ev.Event {
	case "progress":
		var d progressData
		_ = json.Unmarshal(ev.Data, &d)
		c.update(j, func(j *Job) { j.Progress, j.Message = d.Fraction, d.Message })
	case "artifact":
		var d artifactData
		_ = json.Unmarshal(ev.Data, &d)
		c.update(j, func(j *Job) { j.Artifacts = append(j.Artifacts, d.Path) })
	case "done":
		c.update(j, func(j *Job) { j.State, j.Progress, j.Ended = JobDone, 1, time.Now() })
		return true
	case "canceled":
		c.update(j, func(j *Job) { j.State, j.Ended = JobCanceled, time.Now() })
		return true
	case "error":
		var d errorData
		_ = json.Unmarshal(ev.Data, &d)
		c.fail(j, errors.New(d.Message))
		return true
	}
	return false
}

// install 确保引擎环境就绪；同一时间只由调度 goroutine 或设置页触发，状态经 engines 表互斥。
func (c *Conductor) install(ctx context.Context, name string) error {
	c.mu.Lock()
	e := c.engines[name]
	if e.state == EngineReady || e.state == EngineRunning {
		c.mu.Unlock()
		return nil
	}
	if e.state == EngineInstalling {
		c.mu.Unlock()
		return c.waitInstalled(ctx, name)
	}
	e.state, e.err, e.log = EngineInstalling, "", nil
	c.mu.Unlock()
	c.onChange()

	logf := func(line string) {
		c.mu.Lock()
		e.log = append(e.log, line)
		if len(e.log) > logLimit {
			e.log = e.log[len(e.log)-logLimit:]
		}
		c.mu.Unlock()
		c.onChange()
	}
	err := c.layout.Ensure()
	var uv *UV
	if err == nil {
		if c.UV() == nil {
			logf(fmt.Sprintf("未找到 uv，下载 %s", UVVersion))
		}
		uv, err = c.EnsureUV(ctx)
	}
	if err == nil {
		err = Install(ctx, c.src, c.layout, uv, e.spec, logf, c.env()...)
	}

	c.mu.Lock()
	if err != nil {
		e.state, e.err = EngineFailed, err.Error()
	} else {
		e.state, e.outdated = EngineReady, false
	}
	c.mu.Unlock()
	c.onChange()
	return err
}

// waitInstalled 等待另一处触发的安装结束（轮询间隔 200 ms；安装通常持续数秒到数分钟）。
func (c *Conductor) waitInstalled(ctx context.Context, name string) error {
	for {
		c.mu.Lock()
		st, msg := c.engines[name].state, c.engines[name].err
		c.mu.Unlock()
		switch st {
		case EngineReady, EngineRunning:
			return nil
		case EngineFailed:
			return errors.New(msg)
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(200 * time.Millisecond):
		}
	}
}

// workerFor 返回引擎的 worker：复用同引擎的 worker，否则关闭旧 worker 再启动新的。
func (c *Conductor) workerFor(name string) (*Worker, error) {
	c.mu.Lock()
	w, owner := c.worker, c.wEngine
	c.mu.Unlock()
	if w != nil && owner == name && w.Err() == nil {
		select {
		case <-w.Exited:
		default:
			return w, nil
		}
	}
	if w != nil {
		w.Close(5 * time.Second)
		c.setEngineState(owner, EngineReady)
	}

	c.mu.Lock()
	spec := c.engines[name].spec
	c.mu.Unlock()
	w, err := StartWorker(c.layout, spec, c.env()...)
	if err != nil {
		c.mu.Lock()
		c.worker, c.wEngine = nil, ""
		c.engines[name].state, c.engines[name].err = EngineFailed, err.Error()
		c.mu.Unlock()
		c.onChange()
		return nil, err
	}
	c.mu.Lock()
	c.worker, c.wEngine = w, name
	c.mu.Unlock()
	c.setEngineState(name, EngineRunning)
	return w, nil
}

// dropWorker 在 worker 意外退出后清除记录，下次任务重新启动。
func (c *Conductor) dropWorker(w *Worker) {
	c.mu.Lock()
	if c.worker == w {
		c.engines[c.wEngine].state = EngineReady
		c.worker, c.wEngine = nil, ""
	}
	c.mu.Unlock()
	c.onChange()
}

func (c *Conductor) setEngineState(name string, st EngineState) {
	c.mu.Lock()
	if e, ok := c.engines[name]; ok {
		e.state, e.err = st, ""
	}
	c.mu.Unlock()
	c.onChange()
}

func contains(list []string, s string) bool {
	for _, x := range list {
		if x == s {
			return true
		}
	}
	return false
}
