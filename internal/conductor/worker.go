package conductor

import (
	"bufio"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"sync"
	"time"

	"kanade/internal/paths"
)

// ProtocolVersion 是与 engines/sdk 约定的协议版本，worker 的 hello 必须一致。
const ProtocolVersion = 1

// Event 是 worker 上报的一条消息，格式见 engines/sdk/src/kanade_engine/__init__.py。
type Event struct {
	Event string          `json:"event"`
	Job   string          `json:"job,omitempty"`
	Data  json.RawMessage `json:"data,omitempty"`
}

// Hello 是 worker 启动后的第一条事件。
type Hello struct {
	Engine   string   `json:"engine"`
	Version  string   `json:"version"`
	Tasks    []string `json:"tasks"`
	Protocol int      `json:"protocol"`
}

type progressData struct {
	Fraction float64 `json:"fraction"`
	Message  string  `json:"message"`
}

type artifactData struct {
	Path string `json:"path"`
	Kind string `json:"kind"`
}

type errorData struct {
	Message string `json:"message"`
}

// request 是发给 worker 的一行请求。
type request struct {
	Method    string         `json:"method"`
	Job       string         `json:"job,omitempty"`
	Task      string         `json:"task,omitempty"`
	Params    map[string]any `json:"params,omitempty"`
	OutputDir string         `json:"output_dir,omitempty"`
}

// helloTimeout 是等待 worker hello 的上限：包含 Python 启动与导入引擎模块（未来含 torch，冷启动可达十几秒）。
const helloTimeout = 60 * time.Second

// Worker 是一个运行中的引擎进程。
//
// 生命周期：StartWorker 返回时已收到 hello；Events 在进程退出后关闭；Close 幂等。
// send 由 mu 串行化，可从任意 goroutine 调用。
type Worker struct {
	Hello  Hello
	Events <-chan Event
	// Exited 在进程退出后关闭，此后 Err 返回退出原因。
	Exited <-chan struct{}

	cmd     *exec.Cmd
	mu      sync.Mutex
	stdin   io.WriteCloser
	exitErr error
	closing sync.Once
}

// StartWorker 用引擎虚拟环境的 Python 启动 worker，等待 hello 并校验协议版本。
//
// env 为追加的环境变量（镜像设置）。worker 的 stderr（含所有日志与 traceback）追加写入 l.Logs/<engine>.log。
// 启动失败、超时或协议不符时结束进程并返回错误。
func StartWorker(l paths.Layout, spec Spec, env ...string) (*Worker, error) {
	cmd := exec.Command(VenvPython(l, spec.Name), "-m", spec.Module)
	cmd.Dir = Dir(l, spec.Name)
	cmd.Env = Env(l, env...)
	prepareWorker(cmd)

	logFile, err := os.OpenFile(filepath.Join(l.Logs, spec.Name+".log"), os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o644)
	if err != nil {
		return nil, err
	}
	fmt.Fprintf(logFile, "\n=== %s 启动 worker ===\n", time.Now().Format(time.RFC3339))
	cmd.Stderr = logFile
	stdin, err := cmd.StdinPipe()
	if err != nil {
		logFile.Close()
		return nil, err
	}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		logFile.Close()
		return nil, err
	}
	if err := cmd.Start(); err != nil {
		logFile.Close()
		return nil, fmt.Errorf("启动 worker: %w", err)
	}
	if err := adoptWorker(cmd); err != nil {
		// 只影响崩溃时的兜底清理，worker 仍可用；记录后继续。
		fmt.Fprintf(logFile, "加入 Job Object 失败：%v\n", err)
	}

	events := make(chan Event, 64)
	exited := make(chan struct{})
	w := &Worker{Events: events, Exited: exited, cmd: cmd, stdin: stdin}

	// 读取协议流直到 worker 关闭 stdout，然后回收进程并关闭两个通道。
	go func() {
		sc := bufio.NewScanner(stdout)
		sc.Buffer(make([]byte, 64*1024), 4*1024*1024)
		for sc.Scan() {
			var ev Event
			if err := json.Unmarshal(sc.Bytes(), &ev); err != nil {
				fmt.Fprintf(logFile, "无法解析的协议行：%q\n", sc.Text())
				continue
			}
			events <- ev
		}
		err := cmd.Wait()
		logFile.Close()
		w.mu.Lock()
		w.exitErr = err
		w.mu.Unlock()
		close(events)
		close(exited)
	}()

	// 等待 hello：它必须是第一条事件。
	select {
	case ev, ok := <-events:
		if !ok {
			return nil, fmt.Errorf("worker 未发送 hello 就退出了（%v），详见 %s.log", w.Err(), spec.Name)
		}
		if ev.Event != "hello" {
			w.Kill()
			return nil, fmt.Errorf("worker 第一条事件是 %q，期望 hello", ev.Event)
		}
		if err := json.Unmarshal(ev.Data, &w.Hello); err != nil {
			w.Kill()
			return nil, fmt.Errorf("解析 hello: %w", err)
		}
		if w.Hello.Protocol != ProtocolVersion {
			w.Kill()
			return nil, fmt.Errorf("worker 协议版本 %d，期望 %d", w.Hello.Protocol, ProtocolVersion)
		}
	case <-time.After(helloTimeout):
		w.Kill()
		return nil, fmt.Errorf("等待 worker hello 超时（%s）", helloTimeout)
	}
	return w, nil
}

func (w *Worker) send(r request) error {
	data, err := json.Marshal(r)
	if err != nil {
		return err
	}
	w.mu.Lock()
	defer w.mu.Unlock()
	_, err = w.stdin.Write(append(data, '\n'))
	return err
}

// Run 请求 worker 执行任务；结果经 Events 异步返回。
func (w *Worker) Run(job, task string, params map[string]any, outputDir string) error {
	return w.send(request{Method: "run", Job: job, Task: task, Params: params, OutputDir: outputDir})
}

// Cancel 请求取消任务；worker 在下一个检查点响应并发送 canceled。
func (w *Worker) Cancel(job string) error {
	return w.send(request{Method: "cancel", Job: job})
}

// Err 返回进程退出原因；进程仍在运行时为 nil。
func (w *Worker) Err() error {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.exitErr
}

// Close 请求 worker 正常退出，grace 内未退出则强制结束。幂等。
func (w *Worker) Close(grace time.Duration) {
	w.closing.Do(func() {
		_ = w.send(request{Method: "shutdown"})
		w.mu.Lock()
		_ = w.stdin.Close()
		w.mu.Unlock()
		select {
		case <-w.Exited:
		case <-time.After(grace):
			killWorker(w.cmd)
		}
	})
}

// Kill 立即结束 worker 进程（及其进程组）。
func (w *Worker) Kill() {
	killWorker(w.cmd)
}

// errWorkerExited 表示任务执行期间 worker 意外退出。
var errWorkerExited = errors.New("worker 意外退出")
