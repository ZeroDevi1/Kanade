package conductor

import (
	"archive/tar"
	"archive/zip"
	"bytes"
	"compress/gzip"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"kanade/internal/paths"
)

// fakeUVArchive 构造与当前平台发布包布局一致的压缩包，其中 uv 的内容为 payload。
func fakeUVArchive(t *testing.T, payload []byte) []byte {
	t.Helper()
	var buf bytes.Buffer
	if runtime.GOOS == "windows" {
		zw := zip.NewWriter(&buf)
		f, _ := zw.Create("uv.exe")
		f.Write(payload)
		zw.Close()
		return buf.Bytes()
	}
	gz := gzip.NewWriter(&buf)
	tw := tar.NewWriter(gz)
	tw.WriteHeader(&tar.Header{Name: "uv-target/uv", Mode: 0o755, Size: int64(len(payload)), Typeflag: tar.TypeReg})
	tw.Write(payload)
	tw.Close()
	gz.Close()
	return buf.Bytes()
}

func withFakeRelease(t *testing.T, archive []byte, checksum string) {
	t.Helper()
	key := runtime.GOOS + "/" + runtime.GOARCH
	pkg := uvChecksums[key]
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !strings.HasSuffix(r.URL.Path, "/"+UVVersion+"/"+pkg.asset) {
			http.NotFound(w, r)
			return
		}
		w.Write(archive)
	}))
	oldBase, oldPkg := uvReleaseBase, pkg
	uvReleaseBase = srv.URL + "/"
	uvChecksums[key] = struct{ asset, sha256 string }{pkg.asset, checksum}
	t.Cleanup(func() {
		srv.Close()
		uvReleaseBase = oldBase
		uvChecksums[key] = oldPkg
	})
}

func TestDownloadUVVerifiesChecksum(t *testing.T) {
	archive := fakeUVArchive(t, []byte("fake-uv"))
	sum := sha256.Sum256(archive)
	withFakeRelease(t, archive, hex.EncodeToString(sum[:]))

	l := paths.New(t.TempDir())
	uv, err := DownloadUV(context.Background(), l, "")
	if err != nil {
		t.Fatal(err)
	}
	got, _ := os.ReadFile(uv.Path)
	if string(got) != "fake-uv" || uv.Path != filepath.Join(l.UVDir, exeName("uv")) {
		t.Errorf("uv at %s holds %q", uv.Path, got)
	}
}

func TestDownloadUVRejectsBadChecksum(t *testing.T) {
	withFakeRelease(t, fakeUVArchive(t, []byte("tampered")), strings.Repeat("0", 64))

	l := paths.New(t.TempDir())
	if _, err := DownloadUV(context.Background(), l, ""); err == nil || !strings.Contains(err.Error(), "校验失败") {
		t.Fatalf("err = %v, want checksum failure", err)
	}
	if _, err := os.Stat(filepath.Join(l.UVDir, exeName("uv"))); !os.IsNotExist(err) {
		t.Errorf("uv written despite bad checksum: %v", err)
	}
}
