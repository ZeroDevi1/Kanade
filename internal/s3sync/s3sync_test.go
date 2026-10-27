package s3sync

import (
	"context"
	"crypto/md5"
	"crypto/sha256"
	"encoding/hex"
	"encoding/xml"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"
)

// AWS 文档中 ListObjects 的签名示例（Signature Version 4 · Examples · GET Bucket (List Objects)）。
func TestSignMatchesAWSExample(t *testing.T) {
	c, err := New(Config{Endpoint: "https://s3.amazonaws.com", AccessKey: "AKIAIOSFODNN7EXAMPLE",
		SecretKey: "wJalrXUtnFEMI/K7MDENG/bPxRfiCYEXAMPLEKEY", Bucket: "examplebucket"})
	if err != nil {
		t.Fatal(err)
	}
	q := canonicalQuery(url.Values{"max-keys": {"2"}, "prefix": {"J"}})
	req, _ := http.NewRequest(http.MethodGet, "https://examplebucket.s3.amazonaws.com/?"+q, nil)
	c.sign(req, "examplebucket.s3.amazonaws.com", "/", q, emptySHA256, time.Date(2013, 5, 24, 0, 0, 0, 0, time.UTC))
	want := "AWS4-HMAC-SHA256 Credential=AKIAIOSFODNN7EXAMPLE/20130524/us-east-1/s3/aws4_request, " +
		"SignedHeaders=host;x-amz-content-sha256;x-amz-date, Signature=34b48302e7b5fa45bde8084f4b7868a86f0a534bc59db6670ed5711ef69dc6f7"
	if got := req.Header.Get("Authorization"); got != want {
		t.Fatalf("Authorization =\n%s\nwant\n%s", got, want)
	}
}

func TestURIEncode(t *testing.T) {
	if got := uriEncode("/b/20260101-歌 (1)/a~b.json", false); got != "/b/20260101-%E6%AD%8C%20%281%29/a~b.json" {
		t.Fatal(got)
	}
	if got := uriEncode("a/b", true); got != "a%2Fb" {
		t.Fatal(got)
	}
}

func TestDecide(t *testing.T) {
	t0 := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	l := &localFile{size: 3, mod: t0}
	st := &fileState{Size: 3, Mod: t0.UnixNano(), ETag: "e1"}
	same := func() bool { return true }
	cases := []struct {
		name   string
		local  *localFile
		remote *Object
		st     *fileState
		same   bool
		want   action
	}{
		{"仅本地", l, nil, nil, false, actUpload},
		{"仅远端", nil, &Object{ETag: "e1"}, nil, false, actDownload},
		{"本地删除且远端未变", nil, &Object{ETag: "e1"}, st, false, actNone},
		{"本地删除但远端已更新", nil, &Object{ETag: "e2"}, st, false, actDownload},
		{"都未变", l, &Object{Size: 3, ETag: "e1"}, st, false, actNone},
		{"本地变", &localFile{size: 4, mod: t0.Add(time.Second)}, &Object{Size: 3, ETag: "e1"}, st, false, actUpload},
		{"远端变", l, &Object{Size: 5, ETag: "e2"}, st, false, actDownload},
		{"两边都变取较新的本地", &localFile{size: 4, mod: t0.Add(2 * time.Hour)}, &Object{ETag: "e2", LastModified: t0.Add(time.Hour)}, st, false, actUpload},
		{"两边都变取较新的远端", &localFile{size: 4, mod: t0.Add(time.Hour)}, &Object{ETag: "e2", LastModified: t0.Add(2 * time.Hour)}, st, false, actDownload},
		{"无状态内容相同", l, &Object{Size: 3, ETag: "x"}, nil, true, actAdopt},
	}
	for _, tc := range cases {
		f := func() bool { return tc.same }
		if tc.same {
			f = same
		}
		if got := decide(tc.local, tc.remote, tc.st, f); got != tc.want {
			t.Errorf("%s: got %d want %d", tc.name, got, tc.want)
		}
	}
}

// fakeS3 是只支持路径式 ListObjectsV2 / GetObject / PutObject 的内存存储桶。
type fakeS3 struct {
	mu   sync.Mutex
	objs map[string][]byte
	mod  map[string]time.Time
	puts int
	gets int
}

func (f *fakeS3) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if !strings.HasPrefix(r.Header.Get("Authorization"), "AWS4-HMAC-SHA256 Credential=ak/") {
		http.Error(w, "unsigned", http.StatusForbidden)
		return
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	key, ok := strings.CutPrefix(r.URL.Path, "/bkt/")
	switch {
	case r.Method == http.MethodGet && (r.URL.Path == "/bkt" || key == ""):
		type content struct {
			Key          string
			Size         int64
			ETag         string
			LastModified time.Time
		}
		var res struct {
			XMLName  xml.Name `xml:"ListBucketResult"`
			Contents []content
		}
		prefix := r.URL.Query().Get("prefix")
		for k, v := range f.objs {
			if strings.HasPrefix(k, prefix) {
				sum := md5.Sum(v)
				res.Contents = append(res.Contents, content{k, int64(len(v)), `"` + hex.EncodeToString(sum[:]) + `"`, f.mod[k]})
			}
		}
		_ = xml.NewEncoder(w).Encode(res)
	case !ok:
		http.NotFound(w, r)
	case r.Method == http.MethodGet:
		v, ok := f.objs[key]
		if !ok {
			w.WriteHeader(http.StatusNotFound)
			io.WriteString(w, "<Error><Code>NoSuchKey</Code><Message>missing</Message></Error>")
			return
		}
		f.gets++
		_, _ = w.Write(v)
	case r.Method == http.MethodPut:
		data, _ := io.ReadAll(r.Body)
		if sum := sha256.Sum256(data); hex.EncodeToString(sum[:]) != r.Header.Get("x-amz-content-sha256") {
			http.Error(w, "bad hash", http.StatusBadRequest)
			return
		}
		f.objs[key], f.mod[key] = data, time.Now()
		f.puts++
		sum := md5.Sum(data)
		w.Header().Set("ETag", `"`+hex.EncodeToString(sum[:])+`"`)
	default:
		http.Error(w, "unsupported", http.StatusMethodNotAllowed)
	}
}

func (f *fakeS3) keys() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	var out []string
	for k := range f.objs {
		out = append(out, k)
	}
	slices.Sort(out)
	return out
}

func write(t *testing.T, root, rel, data string) {
	t.Helper()
	p := filepath.Join(root, filepath.FromSlash(rel))
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(p, []byte(data), 0o644); err != nil {
		t.Fatal(err)
	}
}

func TestSyncBetweenTwoMachines(t *testing.T) {
	srv := &fakeS3{objs: map[string][]byte{}, mod: map[string]time.Time{}}
	ts := httptest.NewServer(srv)
	defer ts.Close()
	c, err := New(Config{Endpoint: ts.URL, AccessKey: "ak", SecretKey: "sk", Bucket: "bkt", Region: "auto",
		Prefix: "/kanade/", PathStyle: true, Concurrency: 3})
	if err != nil {
		t.Fatal(err)
	}
	// 只同步 rec 开头的目录，跳过 stem.wav；record.json 最后传。
	opts := func(root string) Options {
		return Options{Root: root, Remote: "hist", StateFile: filepath.Join(root, ".state.json"),
			Include: func(rel string) bool { return strings.HasPrefix(rel, "rec") && path.Base(rel) != "stem.wav" },
			Last:    func(rel string) bool { return path.Base(rel) == "record.json" }}
	}
	sync := func(root string) Result {
		t.Helper()
		res, err := Sync(context.Background(), c, opts(root))
		if err != nil {
			t.Fatal(err)
		}
		if len(res.Failed) > 0 {
			t.Fatal(res.Failed)
		}
		return res
	}

	a, b := t.TempDir(), t.TempDir()
	write(t, a, "rec-歌 (1)/record.json", "{}")
	write(t, a, "rec-歌 (1)/analysis.json", "A1")
	write(t, a, "rec-歌 (1)/stem.wav", "big")
	write(t, a, "tmp/analysis.json", "partial")

	if res := sync(a); res.Uploaded != 2 || res.Downloaded != 0 {
		t.Fatalf("首次上传：%+v", res)
	}
	if got := srv.keys(); !slices.Equal(got, []string{"kanade/hist/rec-歌 (1)/analysis.json", "kanade/hist/rec-歌 (1)/record.json"}) {
		t.Fatalf("远端对象：%v", got)
	}
	if res := sync(a); res.Uploaded+res.Downloaded != 0 {
		t.Fatalf("没有变化时不应传输：%+v", res)
	}

	// 另一台机器下载全部记录。
	if res := sync(b); res.Downloaded != 2 {
		t.Fatalf("B 下载：%+v", res)
	}
	if data, _ := os.ReadFile(filepath.Join(b, "rec-歌 (1)", "analysis.json")); string(data) != "A1" {
		t.Fatalf("B 的 analysis.json = %q", data)
	}

	// B 修改后上传，A 再同步时下载。
	time.Sleep(10 * time.Millisecond)
	write(t, b, "rec-歌 (1)/analysis.json", "B2")
	if res := sync(b); res.Uploaded != 1 || res.Downloaded != 0 {
		t.Fatalf("B 上传修改：%+v", res)
	}
	if res := sync(a); res.Downloaded != 1 || res.Uploaded != 0 {
		t.Fatalf("A 下载修改：%+v", res)
	}
	if data, _ := os.ReadFile(filepath.Join(a, "rec-歌 (1)", "analysis.json")); string(data) != "B2" {
		t.Fatalf("A 的 analysis.json = %q", data)
	}

	// A 删除记录后同步：不重新下载，也不删除远端。
	if err := os.RemoveAll(filepath.Join(a, "rec-歌 (1)")); err != nil {
		t.Fatal(err)
	}
	if res := sync(a); res.Uploaded+res.Downloaded != 0 {
		t.Fatalf("本地删除后同步：%+v", res)
	}
	if _, err := os.Stat(filepath.Join(a, "rec-歌 (1)")); !os.IsNotExist(err) {
		t.Fatal("已删除的记录被重新下载")
	}
	if len(srv.keys()) != 2 {
		t.Fatal("远端对象被删除")
	}

	// 没有同步状态、但本地已有相同内容的机器（例如重装后复制过来）：只记录状态，不传输。
	cdir := t.TempDir()
	write(t, cdir, "rec-歌 (1)/record.json", "{}")
	write(t, cdir, "rec-歌 (1)/analysis.json", "B2")
	puts, gets := srv.puts, srv.gets
	if res := sync(cdir); res.Uploaded+res.Downloaded != 0 || srv.puts != puts || srv.gets != gets {
		t.Fatalf("内容相同时不应传输：%+v", res)
	}
}

func TestSyncReportsServiceError(t *testing.T) {
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusForbidden)
		io.WriteString(w, "<Error><Code>AccessDenied</Code><Message>denied</Message></Error>")
	}))
	defer ts.Close()
	c, _ := New(Config{Endpoint: ts.URL, AccessKey: "ak", SecretKey: "sk", Bucket: "bkt", PathStyle: true})
	root := t.TempDir()
	_, err := Sync(context.Background(), c, Options{Root: root, Remote: "hist", StateFile: filepath.Join(root, ".s")})
	if err == nil || !strings.Contains(err.Error(), "AccessDenied") || strings.Contains(err.Error(), "sk") {
		t.Fatalf("err = %v", err)
	}
}

func TestValidRel(t *testing.T) {
	for rel, want := range map[string]bool{"a/b.json": true, "../x": false, "a//b": false, "a/.part": false, `a\b`: false, "": false} {
		if validRel(rel) != want {
			t.Errorf("validRel(%q) != %v", rel, want)
		}
	}
}
