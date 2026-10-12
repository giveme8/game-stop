package library

import (
	"bytes"
	"crypto/hmac"
	"crypto/sha1"
	"encoding/base64"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"strconv"
	"strings"
	"time"
)

// 阿里云 OSS 的最小客户端：只做这个项目真正需要的三件事——上传对象、删除对象、
// 签一个**带图片预处理**的 GET URL。
//
// 为什么不用官方 SDK：这三件事加起来不到两百行，签名算法是公开且稳定的，而且我们在
// 真实桶上有验证手段（scripts/oss-live-check）。少一个依赖就少一处供应链面，也与仓库
// "能不加依赖就不加"的取向一致（Web 端至今零 JS 依赖）。
//
// 桶是**私有**的：读取一律走签名 URL，对象本身不公开。

// 图片预处理规格（契约：封面 3:2 宽 768，截图宽 1280，统一 webp）。
const (
	CoverProcess = "image/resize,m_fill,w_768,h_512/format,webp/quality,q_85"
	ShotProcess  = "image/resize,w_1280/format,webp/quality,q_85"
)

const (
	DefaultOSSPrefix = "games/"
	DefaultOSSTTL    = time.Hour
	// 包与素材分开两层前缀：同一个桶里 `games/<id>/<sha>/…` 是素材，
	// `packages/<id>/<sha>.<ext>` 是游戏包。分开是为了能对两者设不同的生命周期规则
	// （包大、更新少；素材可以更快过期）。
	DefaultPackagePrefix = "packages/"
)

// OSSConfig 全部来自环境变量；AccessKey 走受限 RAM 用户，只对这个桶的对象有读写删权限。
type OSSConfig struct {
	Bucket          string
	Endpoint        string
	AccessKeyID     string
	AccessKeySecret string
	Prefix          string
	TTL             time.Duration
	Now             func() time.Time
}

type OSSClient struct {
	cfg  OSSConfig
	http *http.Client
}

// AssetStore 是服务端对对象存储的**全部**需求。抽成接口是为了能在不碰真桶的前提下
// 测上传链路（内容寻址的 key、失败回滚、签名 URL 落到响应里）。
type AssetStore interface {
	AssetKey(gameID, role, sha256, ext string) string
	PackageKey(gameID, sha256, ext string) string
	PutObject(key string, body []byte, contentType string) error
	// PutObjectStream 用于游戏包：上限 512 MiB，不能整包读进内存。
	PutObjectStream(key string, r io.Reader, size int64, contentType string) error
	DeleteObject(key string) error
	SignedGetURLNow(key, process string) string
}

// 编译期确认 *OSSClient 满足接口。
var _ AssetStore = (*OSSClient)(nil)

// OSSFromEnv 读环境配置；**只要没配就返回 nil,nil** —— 这样没接 OSS 的环境（本地开发、
// 单元测试、还没配好的部署）会老老实实退回本地磁盘，而不是启动失败或偷偷写空桶。
func OSSFromEnv() (*OSSClient, error) {
	get := os.Getenv
	bucket, endpoint := get("OSS_BUCKET"), get("OSS_ENDPOINT")
	id, secret := get("OSS_ACCESS_KEY_ID"), get("OSS_ACCESS_KEY_SECRET")
	if bucket == "" && endpoint == "" && id == "" && secret == "" {
		return nil, nil
	}
	cfg := OSSConfig{Bucket: bucket, Endpoint: endpoint, AccessKeyID: id, AccessKeySecret: secret, Prefix: get("OSS_PREFIX")}
	if raw := get("OSS_URL_TTL"); raw != "" {
		d, err := time.ParseDuration(raw)
		if err != nil {
			return nil, fmt.Errorf("OSS_URL_TTL 不是合法时长: %w", err)
		}
		cfg.TTL = d
	}
	return NewOSSClient(cfg)
}

func NewOSSClient(cfg OSSConfig) (*OSSClient, error) {
	if cfg.Bucket == "" || cfg.Endpoint == "" || cfg.AccessKeyID == "" || cfg.AccessKeySecret == "" {
		return nil, fmt.Errorf("oss: bucket/endpoint/access key 都必须配置")
	}
	if strings.ContainsAny(cfg.Bucket, "/:") || strings.ContainsAny(cfg.Endpoint, "/:") {
		return nil, fmt.Errorf("oss: bucket 只能是名字、endpoint 只能是域名")
	}
	if cfg.Prefix == "" {
		cfg.Prefix = DefaultOSSPrefix
	}
	if !strings.HasSuffix(cfg.Prefix, "/") {
		cfg.Prefix += "/"
	}
	if cfg.TTL <= 0 {
		cfg.TTL = DefaultOSSTTL
	}
	if cfg.Now == nil {
		cfg.Now = time.Now
	}
	return &OSSClient{cfg: cfg, http: &http.Client{Timeout: 60 * time.Second}}, nil
}

func (c *OSSClient) Bucket() string { return c.cfg.Bucket }
func (c *OSSClient) Prefix() string { return c.cfg.Prefix }

// SignedGetURLNow 用客户端自己的时钟签发——生产路径走这个，测试才需要显式时间。
func (c *OSSClient) SignedGetURLNow(key, process string) string {
	return c.SignedGetURL(key, process, c.cfg.Now())
}

// AssetKey 是结构化的对象路径：<prefix><gameId>/<素材内容 sha 前 16 位>/<名字><扩展名>。
//
// 用内容寻址而不是按名字覆盖：换图就是换前缀，旧图自然留着（与"包不可变"同一套思路），
// 也不会出现"URL 没变但内容变了"导致的缓存不一致。
func (c *OSSClient) AssetKey(gameID, role, sha256, ext string) string {
	digest := sha256
	if len(digest) > 16 {
		digest = digest[:16]
	}
	return c.cfg.Prefix + gameID + "/" + digest + "/" + role + ext
}

// PackageKey 是游戏包的对象路径。用**完整** sha256（不截断）：包比素材重要得多，
// 而且它的 sha 本来就要写进清单给客户端校验，多几个字符换取一眼可核对是值得的。
func (c *OSSClient) PackageKey(gameID, sha256, ext string) string {
	return DefaultPackagePrefix + gameID + "/" + sha256 + ext
}

// SignedGetURL 生成带预处理的签名 URL。process 传空就是原图。
func (c *OSSClient) SignedGetURL(key, process string, now time.Time) string {
	expires := now.Add(c.cfg.TTL).Unix()
	// 规范化资源：子资源按字典序拼接，**值用解码后的原样**（OSS 自己的规则）。
	resource := "/" + c.cfg.Bucket + "/" + key
	if process != "" {
		resource += "?x-oss-process=" + process
	}
	signature := c.sign("GET", expires, "", "", resource)
	q := url.Values{}
	if process != "" {
		q.Set("x-oss-process", process)
	}
	q.Set("OSSAccessKeyId", c.cfg.AccessKeyID)
	q.Set("Expires", strconv.FormatInt(expires, 10))
	q.Set("Signature", signature)
	return "https://" + c.cfg.Bucket + "." + c.cfg.Endpoint + "/" + escapePath(key) + "?" + q.Encode()
}

// PutObject 上传一个对象。Content-Type 会进签名，也被 OSS 记录下来。
func (c *OSSClient) PutObject(key string, body []byte, contentType string) error {
	return c.PutObjectStream(key, bytes.NewReader(body), int64(len(body)), contentType)
}

// PutObjectStream 流式上传：游戏包可能有几百 MiB，签名只覆盖 VERB/Content-Type/Date/资源，
// 不含正文长度，所以流式上传与整体上传的签名完全一致。size 必须准确——OSS 要求带
// Content-Length（不靠 chunked），否则会拒。
func (c *OSSClient) PutObjectStream(key string, r io.Reader, size int64, contentType string) error {
	// 用 NopCloser 包一层，别把调用方的句柄交给 http 客户端。
	//
	// http.NewRequest 遇到 io.ReadCloser 会直接把它当 req.Body，而客户端在请求结束后
	// 必然 Close 掉 req.Body——传进来的 *os.File 就这样被顺手关掉。调用方随后对自己的
	// 文件做 Sync/Close 就报 "file already closed"，上传接口整个 503（实测裸 pck 全中）。
	// 句柄归调用方所有，这里只借来读；Close 必须是空操作。
	// 用 NopCloser 包一层，别把调用方的句柄交给 http 客户端。
	//
	// http.NewRequest 遇到 io.ReadCloser 会直接把它当 req.Body，而客户端在请求结束后
	// 必然 Close 掉 req.Body——传进来的 *os.File 就这样被顺手关掉。调用方随后对自己的
	// 文件做 Sync/Close 就报 "file already closed"，上传接口整个 503（实测裸 pck 全中）。
	// 句柄归调用方所有，这里只借来读；Close 必须是空操作。
	req, err := http.NewRequest(http.MethodPut, "https://"+c.cfg.Bucket+"."+c.cfg.Endpoint+"/"+escapePath(key), io.NopCloser(r))
	if err != nil {
		return err
	}
	req.Header.Set("Date", c.cfg.Now().UTC().Format(http.TimeFormat))
	if contentType != "" {
		req.Header.Set("Content-Type", contentType)
	}
	req.ContentLength = size
	req.Header.Set("Authorization", "OSS "+c.cfg.AccessKeyID+":"+c.sign("PUT", 0, "", contentType, "/"+c.cfg.Bucket+"/"+key))
	resp, err := c.http.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode/100 != 2 {
		return fmt.Errorf("oss: put %s 返回 %d: %s", key, resp.StatusCode, readSnippet(resp.Body))
	}
	return nil
}

// DeleteObject 删除对象。**不存在也算成功**：删一个本来就没有的东西不是错误，
// 否则重传时清理旧素材会莫名其妙失败。
func (c *OSSClient) DeleteObject(key string) error {
	req, err := http.NewRequest(http.MethodDelete, "https://"+c.cfg.Bucket+"."+c.cfg.Endpoint+"/"+escapePath(key), nil)
	if err != nil {
		return err
	}
	req.Header.Set("Date", c.cfg.Now().UTC().Format(http.TimeFormat))
	req.Header.Set("Authorization", "OSS "+c.cfg.AccessKeyID+":"+c.sign("DELETE", 0, "", "", "/"+c.cfg.Bucket+"/"+key))
	resp, err := c.http.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode == http.StatusNotFound {
		return nil
	}
	if resp.StatusCode/100 != 2 {
		return fmt.Errorf("oss: delete %s 返回 %d: %s", key, resp.StatusCode, readSnippet(resp.Body))
	}
	return nil
}

// sign 是 OSS 的 V1 签名：StringToSign =
// VERB\nContent-MD5\nContent-Type\nDate\nCanonicalizedResource。
// expires 非 0 时用于 URL 签名（Date 位置放过期时间戳）。
func (c *OSSClient) sign(verb string, expires int64, contentMD5, contentType, resource string) string {
	date := ""
	if expires > 0 {
		date = strconv.FormatInt(expires, 10)
	} else {
		date = c.cfg.Now().UTC().Format(http.TimeFormat)
	}
	stringToSign := strings.Join([]string{verb, contentMD5, contentType, date, resource}, "\n")
	mac := hmac.New(sha1.New, []byte(c.cfg.AccessKeySecret))
	mac.Write([]byte(stringToSign))
	return base64.StdEncoding.EncodeToString(mac.Sum(nil))
}

// escapePath 逐段转义路径：key 里可能有中文等字符，但 "/" 必须保留。
func escapePath(key string) string {
	parts := strings.Split(key, "/")
	for i, p := range parts {
		parts[i] = url.PathEscape(p)
	}
	return strings.Join(parts, "/")
}

func readSnippet(r io.Reader) string {
	b, _ := io.ReadAll(io.LimitReader(r, 400))
	s := strings.TrimSpace(string(b))
	if len(s) > 300 {
		s = s[:300]
	}
	// 只回带 OSS 的 <Code>/<Message>，不要把整段 XML 塞进日志。
	if i := strings.Index(s, "<Code>"); i >= 0 {
		if j := strings.Index(s, "</Message>"); j > i {
			return s[i : j+len("</Message>")]
		}
	}
	return s
}

// AssetRole 把素材在包里的位置映射成对象名（cover / shot-1 …）。
func AssetRole(index int) string {
	if index == 0 {
		return "cover"
	}
	return "shot-" + strconv.Itoa(index)
}

// Process 给出该对象名对应的预处理规格。
func Process(role string) string {
	if role == "cover" {
		return CoverProcess
	}
	return ShotProcess
}
