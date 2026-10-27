// Package s3sync 是 S3 兼容对象存储（AWS S3、Cloudflare R2、MinIO 等）的最小客户端与目录增量同步。
//
// 客户端只实现同步用到的 ListObjectsV2、GetObject、PutObject，签名按 AWS Signature Version 4
// （https://docs.aws.amazon.com/AmazonS3/latest/API/sig-v4-header-based-auth.html），不引入 SDK 依赖。
package s3sync

import (
	"context"
	"crypto/hmac"
	"crypto/md5"
	"crypto/sha256"
	"crypto/tls"
	"encoding/hex"
	"encoding/xml"
	"errors"
	"fmt"
	"io"
	"maps"
	"net"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"time"
)

// Config 是连接一个存储桶所需的参数，与设置文件中的 s3 项对应。
type Config struct {
	// Endpoint 是服务地址，例如 https://<account>.r2.cloudflarestorage.com；省略协议时按 https。
	Endpoint  string `json:"endpoint,omitempty"`
	AccessKey string `json:"accessKey,omitempty"`
	// SecretKey 以明文保存在本机设置文件中，只用于计算签名，不写入日志与错误信息。
	SecretKey string `json:"secretKey,omitempty"`
	Bucket    string `json:"bucket,omitempty"`
	// Region 为空时按 us-east-1；Cloudflare R2 填 auto。
	Region string `json:"region,omitempty"`
	// Prefix 是对象键的前缀（不含首尾斜杠），用于和同一存储桶中的其他数据分开；空为存储桶根。
	Prefix string `json:"prefix,omitempty"`
	// PathStyle 为 true 时地址为 endpoint/bucket/key，否则为 bucket.endpoint/key（虚拟主机）。
	PathStyle bool `json:"pathStyle,omitempty"`
	// SkipTLSVerify 为 true 时不校验服务器证书（自签名证书的自建服务）。
	SkipTLSVerify bool `json:"skipTLSVerify,omitempty"`
	// Timeout 是建立连接与等待响应头的上限（秒），不限制传输大文件的总时长；≤0 按 DefaultTimeout。
	Timeout int `json:"timeout,omitempty"`
	// Concurrency 是同时进行的上传 / 下载数；≤0 按 DefaultConcurrency。
	Concurrency int `json:"concurrency,omitempty"`
}

const (
	DefaultTimeout     = 60
	DefaultConcurrency = 4
	// MaxConcurrency 限制并发数：分析记录中的文件多为几 MB 到几十 MB，更高并发对带宽没有帮助。
	MaxConcurrency = 16
	defaultRegion  = "us-east-1"
)

// Configured 报告必填项（地址、密钥、存储桶）是否都已填写。
func (c Config) Configured() bool {
	return strings.TrimSpace(c.Endpoint) != "" && c.AccessKey != "" && c.SecretKey != "" && strings.TrimSpace(c.Bucket) != ""
}

// Client 是一个存储桶的客户端，创建后只读，可并发使用。
type Client struct {
	cfg      Config
	endpoint *url.URL
	http     *http.Client
}

// New 校验配置并创建客户端；地址无法解析或必填项为空时返回错误。
func New(cfg Config) (*Client, error) {
	if !cfg.Configured() {
		return nil, errors.New("S3 配置不完整：需要 Endpoint、Access Key、Secret Key 与 Bucket")
	}
	ep := strings.TrimSpace(cfg.Endpoint)
	if !strings.Contains(ep, "://") {
		ep = "https://" + ep
	}
	u, err := url.Parse(strings.TrimRight(ep, "/"))
	if err != nil || u.Host == "" || (u.Scheme != "https" && u.Scheme != "http") {
		return nil, fmt.Errorf("Endpoint 不是有效的 http(s) 地址：%s", cfg.Endpoint)
	}
	if u.Path != "" || u.RawQuery != "" {
		return nil, fmt.Errorf("Endpoint 只填服务地址，不含路径：%s", cfg.Endpoint)
	}
	cfg.Bucket = strings.TrimSpace(cfg.Bucket)
	cfg.Region = strings.TrimSpace(cfg.Region)
	if cfg.Region == "" {
		cfg.Region = defaultRegion
	}
	cfg.Prefix = strings.Trim(strings.TrimSpace(cfg.Prefix), "/")
	timeout := time.Duration(cfg.Timeout) * time.Second
	if cfg.Timeout <= 0 {
		timeout = DefaultTimeout * time.Second
	}
	tr := http.DefaultTransport.(*http.Transport).Clone()
	tr.DialContext = (&net.Dialer{Timeout: timeout, KeepAlive: 30 * time.Second}).DialContext
	tr.TLSHandshakeTimeout = timeout
	tr.ResponseHeaderTimeout = timeout
	if cfg.SkipTLSVerify {
		tr.TLSClientConfig = &tls.Config{InsecureSkipVerify: true}
	}
	return &Client{cfg: cfg, endpoint: u, http: &http.Client{Transport: tr}}, nil
}

// Key 返回 rel（以 / 分隔的相对路径）加上前缀后的对象键。
func (c *Client) Key(rel string) string {
	if c.cfg.Prefix == "" {
		return rel
	}
	return c.cfg.Prefix + "/" + rel
}

// Identity 标识客户端指向的位置（地址、存储桶与前缀）；同步状态只对同一位置有效。
func (c *Client) Identity() string {
	return c.endpoint.String() + "|" + c.cfg.Bucket + "|" + c.cfg.Prefix
}

// Object 是列举得到的一个对象。
type Object struct {
	Key          string
	Size         int64
	ETag         string // 去掉引号；单次上传的对象为内容的 MD5（十六进制）
	LastModified time.Time
}

type listResult struct {
	Contents []struct {
		Key          string    `xml:"Key"`
		Size         int64     `xml:"Size"`
		ETag         string    `xml:"ETag"`
		LastModified time.Time `xml:"LastModified"`
	} `xml:"Contents"`
	IsTruncated           bool   `xml:"IsTruncated"`
	NextContinuationToken string `xml:"NextContinuationToken"`
}

// List 列出键以 prefix 开头的全部对象（自动翻页）。
func (c *Client) List(ctx context.Context, prefix string) ([]Object, error) {
	var out []Object
	token := ""
	for {
		q := url.Values{"list-type": {"2"}, "prefix": {prefix}}
		if token != "" {
			q.Set("continuation-token", token)
		}
		resp, err := c.do(ctx, http.MethodGet, "", q, nil, emptySHA256, 0)
		if err != nil {
			return nil, err
		}
		var res listResult
		err = xml.NewDecoder(resp.Body).Decode(&res)
		resp.Body.Close()
		if err != nil {
			return nil, fmt.Errorf("解析对象列表：%w", err)
		}
		for _, o := range res.Contents {
			out = append(out, Object{Key: o.Key, Size: o.Size, ETag: strings.Trim(o.ETag, `"`), LastModified: o.LastModified})
		}
		if !res.IsTruncated || res.NextContinuationToken == "" {
			return out, nil
		}
		token = res.NextContinuationToken
	}
}

// Download 把对象 key 下载到 dst：先写同目录的临时文件，完整后再替换，失败时不留下半截文件。
// 返回响应中的 ETag（去掉引号）。
func (c *Client) Download(ctx context.Context, key, dst string) (string, error) {
	resp, err := c.do(ctx, http.MethodGet, key, nil, nil, emptySHA256, 0)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	if err := os.MkdirAll(filepath.Dir(dst), 0o755); err != nil {
		return "", err
	}
	tmp, err := os.CreateTemp(filepath.Dir(dst), "."+filepath.Base(dst)+".*.part")
	if err != nil {
		return "", err
	}
	_, err = io.Copy(tmp, resp.Body)
	if cerr := tmp.Close(); err == nil {
		err = cerr
	}
	if err == nil {
		err = os.Rename(tmp.Name(), dst)
	}
	if err != nil {
		os.Remove(tmp.Name())
		return "", err
	}
	return strings.Trim(resp.Header.Get("ETag"), `"`), nil
}

// Upload 把本地文件 src 上传为对象 key，返回新对象的 ETag（去掉引号；服务端未返回时为内容的 MD5，
// 与单次上传对象的 ETag 一致）。先读一遍计算哈希写入签名，再读第二遍作为请求体，不把整个文件读入内存。
func (c *Client) Upload(ctx context.Context, key, src string) (string, error) {
	f, err := os.Open(src)
	if err != nil {
		return "", err
	}
	defer f.Close()
	h, m := sha256.New(), md5.New()
	size, err := io.Copy(io.MultiWriter(h, m), f)
	if err != nil {
		return "", err
	}
	if _, err := f.Seek(0, io.SeekStart); err != nil {
		return "", err
	}
	resp, err := c.do(ctx, http.MethodPut, key, nil, f, hex.EncodeToString(h.Sum(nil)), size)
	if err != nil {
		return "", err
	}
	resp.Body.Close()
	if etag := strings.Trim(resp.Header.Get("ETag"), `"`); etag != "" {
		return etag, nil
	}
	return hex.EncodeToString(m.Sum(nil)), nil
}

// emptySHA256 是空请求体的 SHA-256。
const emptySHA256 = "e3b0c44298fc1c149afbf4c8996fb92427ae41e4649b934ca495991b7852b855"

// do 发送签名后的请求；key 为空时请求存储桶本身。非 2xx 响应转为 *Error 返回（已关闭响应体）。
func (c *Client) do(ctx context.Context, method, key string, q url.Values, body io.Reader, payloadHash string, size int64) (*http.Response, error) {
	host, path := c.endpoint.Host, "/"+c.cfg.Bucket
	if !c.cfg.PathStyle {
		host, path = c.cfg.Bucket+"."+host, ""
	}
	// 路径式访问存储桶本身时用 /bucket（不带结尾斜杠），虚拟主机式用 /。
	if key != "" || !c.cfg.PathStyle {
		path += "/" + key
	}
	encPath, encQuery := uriEncode(path, false), canonicalQuery(q)
	raw := c.endpoint.Scheme + "://" + host + encPath
	if encQuery != "" {
		raw += "?" + encQuery
	}
	req, err := http.NewRequestWithContext(ctx, method, raw, body)
	if err != nil {
		return nil, err
	}
	// 显式使用签名时的编码：Go 默认的路径编码与 SigV4 的规范编码在 !、'、( 等字符上不同。
	req.URL.RawPath = encPath
	if body != nil {
		req.ContentLength = size
	}
	c.sign(req, host, encPath, encQuery, payloadHash, time.Now().UTC())
	resp, err := c.http.Do(req)
	if err != nil {
		return nil, err
	}
	if resp.StatusCode/100 != 2 {
		defer resp.Body.Close()
		return nil, parseError(resp)
	}
	return resp, nil
}

// sign 按 SigV4 为请求加上 x-amz-date、x-amz-content-sha256 与 Authorization 头。
func (c *Client) sign(req *http.Request, host, encPath, encQuery, payloadHash string, now time.Time) {
	amzDate, date := now.Format("20060102T150405Z"), now.Format("20060102")
	req.Header.Set("x-amz-date", amzDate)
	req.Header.Set("x-amz-content-sha256", payloadHash)
	const signed = "host;x-amz-content-sha256;x-amz-date"
	canonical := strings.Join([]string{req.Method, encPath, encQuery,
		"host:" + host + "\nx-amz-content-sha256:" + payloadHash + "\nx-amz-date:" + amzDate + "\n",
		signed, payloadHash}, "\n")
	scope := date + "/" + c.cfg.Region + "/s3/aws4_request"
	sum := sha256.Sum256([]byte(canonical))
	toSign := "AWS4-HMAC-SHA256\n" + amzDate + "\n" + scope + "\n" + hex.EncodeToString(sum[:])
	k := hmacSHA256([]byte("AWS4"+c.cfg.SecretKey), date)
	k = hmacSHA256(k, c.cfg.Region)
	k = hmacSHA256(k, "s3")
	k = hmacSHA256(k, "aws4_request")
	req.Header.Set("Authorization", "AWS4-HMAC-SHA256 Credential="+c.cfg.AccessKey+"/"+scope+
		", SignedHeaders="+signed+", Signature="+hex.EncodeToString(hmacSHA256(k, toSign)))
}

func hmacSHA256(key []byte, data string) []byte {
	m := hmac.New(sha256.New, key)
	m.Write([]byte(data))
	return m.Sum(nil)
}

// uriEncode 按 SigV4 的规则编码：只保留 A-Z a-z 0-9 - _ . ~，其余按 UTF-8 字节写成大写 %XX；
// encodeSlash 为 false 时（路径）保留 /。
func uriEncode(s string, encodeSlash bool) string {
	var b strings.Builder
	for i := 0; i < len(s); i++ {
		ch := s[i]
		switch {
		case 'A' <= ch && ch <= 'Z', 'a' <= ch && ch <= 'z', '0' <= ch && ch <= '9',
			ch == '-', ch == '_', ch == '.', ch == '~', ch == '/' && !encodeSlash:
			b.WriteByte(ch)
		default:
			fmt.Fprintf(&b, "%%%02X", ch)
		}
	}
	return b.String()
}

// canonicalQuery 按键排序并编码查询参数（每个键只取第一个值，本客户端不发送多值参数）。
func canonicalQuery(q url.Values) string {
	keys := slices.Sorted(maps.Keys(q))
	parts := make([]string, 0, len(keys))
	for _, k := range keys {
		parts = append(parts, uriEncode(k, true)+"="+uriEncode(q.Get(k), true))
	}
	return strings.Join(parts, "&")
}

// Error 是服务端返回的错误。
type Error struct {
	Status  int
	Code    string
	Message string
}

func (e *Error) Error() string {
	if e.Code == "" {
		return fmt.Sprintf("S3 请求失败：HTTP %d", e.Status)
	}
	return fmt.Sprintf("S3 请求失败：HTTP %d %s：%s", e.Status, e.Code, e.Message)
}

func parseError(resp *http.Response) error {
	e := &Error{Status: resp.StatusCode}
	var body struct {
		Code    string `xml:"Code"`
		Message string `xml:"Message"`
	}
	data, _ := io.ReadAll(io.LimitReader(resp.Body, 64<<10))
	if xml.Unmarshal(data, &body) == nil {
		e.Code, e.Message = body.Code, body.Message
	}
	return e
}
