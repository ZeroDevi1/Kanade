package s3sync

import (
	"context"
	"crypto/md5"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path"
	"path/filepath"
	"strings"
	"sync"
	"time"
)

// 增量同步：把本地目录与存储桶中的一个前缀双向合并，只传输变化的文件。
//
// 判断依据是本地的同步状态文件，记录每个文件上次同步后的本地大小、修改时间与远端 ETag：
//   - 本地相对上次同步变了、远端没变 → 上传；远端变了、本地没变 → 下载；两边都变 → 修改时间较新的一方覆盖另一方。
//   - 只在一侧存在且从未同步过 → 复制到另一侧。
//   - 上次同步过、本地已删除且远端未变 → 视为本地有意删除，不再下载；远端对象保留，同步不删除任何远端数据。
//   - 两边都有但没有状态（首次同步或换了存储位置）→ 大小相同且本地 MD5 等于 ETag 时只记录状态，不传输。
//
// 同一时刻只应有一个 Sync 作用于同一本地目录（状态文件整体读写，不加文件锁）。

// Options 描述一次同步。
type Options struct {
	// Root 是本地目录；Remote 是对象键中对应的相对前缀（加在客户端的 Prefix 之后），不含首尾斜杠。
	Root, Remote string
	// StateFile 是同步状态文件的路径；放在 Root 内时须以 . 开头，才不会被当成要同步的文件。
	StateFile string
	// Include 决定本地文件 rel（相对 Root、以 / 分隔）是否参与同步；nil 为全部。
	// 被排除的本地文件也不会被远端同名对象覆盖。
	Include func(rel string) bool
	// Last 为 true 的文件在其他文件都传输完后才传输（例如目录的描述文件，保证它出现时同目录的文件已完整）。
	Last func(rel string) bool
	// Progress 在每传输完一个文件后调用（任意 goroutine），done ≤ total。
	Progress func(done, total int)
}

// Result 是一次同步的统计。
type Result struct {
	Uploaded, Downloaded int
	// Failed 是传输失败的文件与原因；不影响其余文件。
	Failed []string
}

type fileState struct {
	Size int64  `json:"size"`
	Mod  int64  `json:"mod"` // 本地修改时间（Unix 纳秒）
	ETag string `json:"etag"`
}

type syncState struct {
	// Remote 是 Client.Identity；与当前客户端不同时整个状态作废（换了存储位置）。
	Remote string               `json:"remote"`
	Files  map[string]fileState `json:"files"`
}

type localFile struct {
	size int64
	mod  time.Time
}

type action int

const (
	actNone action = iota
	actUpload
	actDownload
	actAdopt // 两边内容相同，只记录状态
)

// decide 决定一个文件的处理方式；local / remote 为 nil 表示该侧不存在，st 为 nil 表示从未同步。
// sameContent 只在两边都存在且没有状态时调用，用于首次同步时避免重复传输相同的文件。
func decide(local *localFile, remote *Object, st *fileState, sameContent func() bool) action {
	newer := func() action {
		if local.mod.After(remote.LastModified) {
			return actUpload
		}
		return actDownload
	}
	switch {
	case local == nil && remote == nil:
		return actNone
	case remote == nil:
		return actUpload
	case local == nil:
		if st != nil && st.ETag == remote.ETag {
			return actNone
		}
		return actDownload
	case st == nil:
		if local.size == remote.Size && sameContent() {
			return actAdopt
		}
		return newer()
	}
	lc := local.size != st.Size || local.mod.UnixNano() != st.Mod
	rc := remote.ETag != st.ETag
	switch {
	case lc && rc:
		return newer()
	case lc:
		return actUpload
	case rc:
		return actDownload
	}
	return actNone
}

// Sync 按 opt 双向同步一次。列举远端或读写状态文件失败时返回错误；单个文件传输失败记在 Result.Failed 中。
// ctx 取消时停止派发新的传输，已完成部分的状态仍会保存，下次同步从剩余部分继续。
func Sync(ctx context.Context, c *Client, opt Options) (Result, error) {
	var res Result
	st, err := loadState(opt.StateFile, c.Identity())
	if err != nil {
		return res, err
	}
	locals, excluded, err := scanLocal(opt)
	if err != nil {
		return res, err
	}
	prefix := c.Key(opt.Remote) + "/"
	objs, err := c.List(ctx, prefix)
	if err != nil {
		return res, err
	}
	remotes := map[string]*Object{}
	for i := range objs {
		rel := strings.TrimPrefix(objs[i].Key, prefix)
		if validRel(rel) {
			remotes[rel] = &objs[i]
		}
	}

	// 逐个文件决定处理方式，需要传输的分成两批（Last 的放第二批）。
	type task struct {
		rel string
		act action
	}
	var first, last []task
	seen := map[string]bool{}
	for rel := range locals {
		seen[rel] = true
	}
	for rel := range remotes {
		seen[rel] = true
	}
	for rel := range seen {
		if excluded[rel] {
			continue
		}
		l, r := locals[rel], remotes[rel]
		var fs *fileState
		if s, ok := st.Files[rel]; ok {
			fs = &s
		}
		act := decide(l, r, fs, func() bool { return sameMD5(filepath.Join(opt.Root, filepath.FromSlash(rel)), r.ETag) })
		switch act {
		case actNone:
			if l == nil && r == nil {
				delete(st.Files, rel)
			}
		case actAdopt:
			st.Files[rel] = fileState{Size: l.size, Mod: l.mod.UnixNano(), ETag: r.ETag}
		default:
			if opt.Last != nil && opt.Last(rel) {
				last = append(last, task{rel, act})
			} else {
				first = append(first, task{rel, act})
			}
		}
	}
	// 状态里有、两边都已不存在的项删掉，避免状态文件无限增长。
	for rel := range st.Files {
		if !seen[rel] {
			delete(st.Files, rel)
		}
	}

	// 两批依次并发传输。
	var mu sync.Mutex
	total, done := len(first)+len(last), 0
	// failedDirs 是有文件传输失败的目录：其中的 Last 文件本次不传，保证描述文件出现时同目录的文件已完整。
	failedDirs := map[string]bool{}
	run := func(t task) {
		local := filepath.Join(opt.Root, filepath.FromSlash(t.rel))
		key := prefix + t.rel
		var etag string
		var err error
		if t.act == actUpload {
			etag, err = c.Upload(ctx, key, local)
		} else {
			etag = remotes[t.rel].ETag
			_, err = c.Download(ctx, key, local)
		}
		var info os.FileInfo
		if err == nil {
			info, err = os.Stat(local)
		}
		mu.Lock()
		defer mu.Unlock()
		done++
		switch {
		case err != nil:
			res.Failed = append(res.Failed, t.rel+"："+err.Error())
			failedDirs[path.Dir(t.rel)] = true
		case t.act == actUpload:
			res.Uploaded++
		default:
			res.Downloaded++
		}
		if err == nil {
			st.Files[t.rel] = fileState{Size: info.Size(), Mod: info.ModTime().UnixNano(), ETag: etag}
		}
		if opt.Progress != nil {
			opt.Progress(done, total)
		}
	}
	workers := c.cfg.Concurrency
	if workers <= 0 {
		workers = DefaultConcurrency
	}
	workers = min(workers, MaxConcurrency)
	for bi, batch := range [][]task{first, last} {
		ch := make(chan task)
		var wg sync.WaitGroup
		for range min(workers, len(batch)) {
			wg.Go(func() {
				for t := range ch {
					run(t)
				}
			})
		}
		for _, t := range batch {
			if ctx.Err() != nil {
				break
			}
			mu.Lock()
			skip := bi == 1 && failedDirs[path.Dir(t.rel)]
			mu.Unlock()
			if skip {
				continue
			}
			ch <- t
		}
		close(ch)
		wg.Wait()
	}

	if err := saveState(opt.StateFile, st); err != nil {
		return res, fmt.Errorf("保存同步状态：%w", err)
	}
	return res, ctx.Err()
}

// scanLocal 列出 Root 下的文件：参与同步的放在 locals，被 Include 排除的记在 excluded。
// 以 . 开头的文件与目录（状态文件、下载中的临时文件）不参与。Root 不存在时视为空目录。
func scanLocal(opt Options) (map[string]*localFile, map[string]bool, error) {
	locals, excluded := map[string]*localFile{}, map[string]bool{}
	err := filepath.WalkDir(opt.Root, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			if p == opt.Root && errors.Is(err, fs.ErrNotExist) {
				return filepath.SkipAll
			}
			return err
		}
		if p == opt.Root {
			return nil
		}
		if strings.HasPrefix(d.Name(), ".") {
			if d.IsDir() {
				return filepath.SkipDir
			}
			return nil
		}
		if d.IsDir() || !d.Type().IsRegular() {
			return nil
		}
		rel, err := filepath.Rel(opt.Root, p)
		if err != nil {
			return err
		}
		rel = filepath.ToSlash(rel)
		if opt.Include != nil && !opt.Include(rel) {
			excluded[rel] = true
			return nil
		}
		info, err := d.Info()
		if err != nil {
			return err
		}
		locals[rel] = &localFile{size: info.Size(), mod: info.ModTime()}
		return nil
	})
	return locals, excluded, err
}

// validRel 拒绝不能安全映射到本地路径的对象键：空段、.、..、以 . 开头的名称（与本地扫描规则一致）、反斜杠与冒号。
func validRel(rel string) bool {
	if rel == "" || strings.ContainsAny(rel, `\:`) {
		return false
	}
	for seg := range strings.SplitSeq(rel, "/") {
		if seg == "" || strings.HasPrefix(seg, ".") {
			return false
		}
	}
	return true
}

// sameMD5 报告本地文件的 MD5 是否等于 etag。分段上传的对象 ETag 不是 MD5（含 -），此时返回 false。
func sameMD5(p, etag string) bool {
	if len(etag) != 32 {
		return false
	}
	f, err := os.Open(p)
	if err != nil {
		return false
	}
	defer f.Close()
	h := md5.New()
	if _, err := io.Copy(h, f); err != nil {
		return false
	}
	return strings.EqualFold(hex.EncodeToString(h.Sum(nil)), etag)
}

// loadState 读取状态文件；不存在、或记录的存储位置与 remote 不同时返回空状态。
func loadState(p, remote string) (*syncState, error) {
	st := &syncState{Remote: remote, Files: map[string]fileState{}}
	data, err := os.ReadFile(p)
	if errors.Is(err, fs.ErrNotExist) {
		return st, nil
	}
	if err != nil {
		return nil, err
	}
	var saved syncState
	if err := json.Unmarshal(data, &saved); err != nil {
		return nil, fmt.Errorf("同步状态文件 %s 已损坏，删除后重新同步：%w", p, err)
	}
	if saved.Remote == remote && saved.Files != nil {
		st.Files = saved.Files
	}
	return st, nil
}

// saveState 先写临时文件再重命名，避免中途退出留下损坏的状态文件。
func saveState(p string, st *syncState) error {
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		return err
	}
	data, err := json.MarshalIndent(st, "", "  ")
	if err != nil {
		return err
	}
	tmp := p + ".tmp"
	if err := os.WriteFile(tmp, data, 0o644); err != nil {
		return err
	}
	return os.Rename(tmp, p)
}
