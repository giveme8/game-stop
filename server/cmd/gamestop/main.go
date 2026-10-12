// gamestop is the HTTPS client for the game library. Credentials are read only
// from GAMESTOP_TOKEN, never from command arguments or a persisted config file.
package main

import (
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"time"

	"github.com/giveme8/game-stop/server/internal/library"
)

const maxPackage = int64(512 << 20)

var validID = regexp.MustCompile(`^[a-z0-9][a-z0-9-]{0,63}$`)

// buildVersion 由构建时注入（-ldflags "-X main.buildVersion=..."），本地 go build 时为 dev。
//
// 存在的理由：手上这个二进制到底是哪一版，以前只能靠 `file` 和时间戳去猜——而 CLI 的
// 提示文案在版本之间变过（比如 `.game` 支持前是 "only .pck uploads supported"），
// 猜错方向就会白查半天。
var buildVersion = "dev"

type game struct {
	ID      string `json:"id"`
	Package struct {
		URL    string `json:"url"`
		Size   int64  `json:"size"`
		SHA256 string `json:"sha256"`
	} `json:"package"`
	// 服务端**收到**的字节。容器提交时它与 package.* 不同：收到的是 .game 容器，
	// 对外分发的却是从里面解出来的 pck。上传后要对账的是这一组。
	Received struct {
		Size   int64  `json:"size"`
		SHA256 string `json:"sha256"`
	} `json:"received"`
}

type client struct {
	base, token string
	http        *http.Client
}

func newClient(base, token string) (*client, error) {
	u, err := url.Parse(base)
	if err != nil || u.Scheme != "https" || u.Host == "" || u.User != nil || u.RawQuery != "" || u.Fragment != "" || (u.Path != "" && u.Path != "/") {
		return nil, errors.New("server must be an HTTPS origin without credentials, path, query or fragment")
	}
	return &client{strings.TrimRight(base, "/"), token, &http.Client{Timeout: 30 * time.Minute, CheckRedirect: func(_ *http.Request, _ []*http.Request) error { return http.ErrUseLastResponse }}}, nil
}

// verifyPackage 在**本机**跑一遍服务端会跑的那套校验。
// 发布者不必"传上去才知道哪里不对"，而且不需要 Token、不产生任何服务端副作用。
func verifyPackage(path, id string) error {
	f, err := os.Open(path)
	if err != nil {
		return err
	}
	defer f.Close()
	info, err := f.Stat()
	if err != nil {
		return err
	}
	if !info.Mode().IsRegular() || info.Size() <= 0 || info.Size() > maxPackage {
		return errors.New("package must be a regular file between 1 byte and 512 MiB")
	}
	head := make([]byte, 4)
	if _, err := f.ReadAt(head, 0); err != nil {
		return errors.New("cannot read package header")
	}
	switch {
	case library.LooksLikeBundle(head):
		plan, err := library.InspectBundle(f, info.Size(), id)
		if err != nil {
			return errors.New("invalid or unsafe .game bundle")
		}
		// 入口 pck 仍要过一遍完整校验：容器合法不代表里面的内容合法。
		rc, err := plan.OpenEntry()
		if err != nil {
			return errors.New("bundle entry cannot be opened")
		}
		defer rc.Close()
		tmp, err := os.CreateTemp("", "gamestop-verify-*.pck")
		if err != nil {
			return err
		}
		defer os.Remove(tmp.Name())
		defer tmp.Close()
		n, err := library.CopyLimited(tmp, rc, maxPackage)
		if err != nil {
			return errors.New("bundle entry is too large or unreadable")
		}
		version, err := library.InspectPCK(tmp, n)
		if err != nil {
			return errors.New("bundle entry is not a valid PCK")
		}
		if version != plan.Meta.Engine.Version {
			return fmt.Errorf("engine mismatch: game.json says %s but the pck is %s",
				plan.Meta.Engine.Version, version)
		}
		fmt.Printf("OK  .game bundle: id=%s title=%q engine=%s entry=%s assets=%d\n",
			plan.Meta.ID, plan.Meta.Title, version, plan.Entry, len(plan.Assets))
		if plan.Meta.Source.Repo != "" {
			fmt.Printf("    source: %s %s\n", plan.Meta.Source.Repo, plan.Meta.Source.Ref)
		}
		return nil
	case string(head) == "GDPC":
		version, err := library.InspectPCK(f, info.Size())
		if err != nil {
			return errors.New("invalid or unsafe PCK")
		}
		fmt.Printf("OK  bare pck: engine=%s (remember --metadata on upload)\n", version)
		return nil
	default:
		return errors.New("not a .game bundle (zip) or a pck (GDPC)")
	}
}

func (c *client) request(method, path string, body io.Reader, metadata string, size int64) (*http.Response, error) {
	r, err := http.NewRequest(method, c.base+path, body)
	if err != nil {
		return nil, err
	}
	if c.token != "" {
		r.Header.Set("Authorization", "Bearer "+c.token)
	}
	if method == "PUT" {
		r.Header.Set("Content-Type", "application/octet-stream")
		// 空值不要发出去：服务端把"这个头存在"当作第二个元数据真源并拒收 `.game`，
		// 而空头的语义就是"没有"。发一个空头只会制造一次莫名其妙的 400。
		if metadata != "" {
			r.Header.Set("X-Game-Metadata", metadata)
		}
		r.ContentLength = size
	}
	if method == "PATCH" {
		r.Header.Set("Content-Type", "application/json")
	}
	resp, err := c.http.Do(r)
	if err != nil {
		return nil, fmt.Errorf("HTTPS request failed: %w", err)
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		// 把服务端自己给的 error 文案带出来。
		//
		// 以前只回显状态码，于是 503 在用户眼里就是光秃秃一个"503"——连是我们返回的
		// 哪一种 503（对象存储不可用 / 存储不可用 / 上传槽满 / 认证不可用）都看不出来，
		// 排查只能靠数响应体字节数。这是**我们自己服务端的错误说明**，不是凭据。
		detail := serverError(resp.Body)
		resp.Body.Close()
		if detail != "" {
			return nil, fmt.Errorf("server returned HTTP %d: %s", resp.StatusCode, detail)
		}
		return nil, fmt.Errorf("server returned HTTP %d (401: token invalid/expired; 404: missing or private; 409: ID conflict)", resp.StatusCode)
	}
	return resp, nil
}

// serverError 取出服务端 JSON 里的 error 字段。只读前 4 KiB，拿不到就返回空串
// （调用方退回原来的通用提示）。
func serverError(body io.Reader) string {
	b, err := io.ReadAll(io.LimitReader(body, 4096))
	if err != nil {
		return ""
	}
	var parsed struct {
		Error string `json:"error"`
	}
	if json.Unmarshal(b, &parsed) != nil {
		return ""
	}
	return parsed.Error
}

// fetchPackage 取包体。地址来自清单：可能是服务端签发的对象存储临时地址，
// 也可能是平台自己的端点（老记录、或没配对象存储的部署）。
//
// 规矩没变——**不把凭据发给第三方**：同源才带 Bearer，跨源一律裸请求，
// 因为跨源的授权来自签名本身。同时只接受 https，避免被清单里的明文地址降级。
func (c *client) fetchPackage(rawURL string) (*http.Response, error) {
	u, err := url.Parse(rawURL)
	if err != nil || u.Scheme != "https" || u.Host == "" || u.User != nil {
		return nil, errors.New("package url must be an https address without credentials")
	}
	base, err := url.Parse(c.base)
	if err != nil {
		return nil, err
	}
	req, err := http.NewRequest(http.MethodGet, rawURL, nil)
	if err != nil {
		return nil, err
	}
	if u.Host == base.Host && c.token != "" {
		req.Header.Set("Authorization", "Bearer "+c.token)
	}
	return c.http.Do(req)
}

func (c *client) jsonRequest(method, path string, body io.Reader) ([]byte, error) {
	r, err := c.request(method, path, body, "", 0)
	if err != nil {
		return nil, err
	}
	defer r.Body.Close()
	b, err := io.ReadAll(io.LimitReader(r.Body, (4<<20)+1))
	if err != nil {
		return nil, err
	}
	if len(b) > 4<<20 {
		return nil, errors.New("response too large")
	}
	return b, nil
}

func (c *client) download(id, output string) (err error) {
	b, err := c.jsonRequest("GET", "/api/v1/games/"+id, nil)
	if err != nil {
		return err
	}
	var g game
	if err = json.Unmarshal(b, &g); err != nil {
		return err
	}
	hash, err := hex.DecodeString(g.Package.SHA256)
	if err != nil || len(hash) != 32 || g.Package.Size <= 0 || g.Package.Size > maxPackage || g.ID != id {
		return errors.New("invalid package metadata")
	}
	// 包现在多数在对象存储上（清单里的 package.url 是临时签名地址），但**仍然只把它当
	// 一个地址看**：先确认它是 https，然后
	//   - 与 API 同源 → 带 Bearer（平台自己的端点需要鉴权）；
	//   - 不同源     → 绝不带 Bearer，签名本身就是授权。
	// 这样既支持 OSS，又保住原来那条"不把凭据发给第三方"的规矩。
	r, err := c.fetchPackage(g.Package.URL)
	if err != nil {
		return err
	}
	defer r.Body.Close()
	if r.ContentLength >= 0 && r.ContentLength != g.Package.Size {
		return errors.New("package size changed; retry download")
	}
	f, err := os.CreateTemp(filepath.Dir(output), ".gamestop-*.part")
	if err != nil {
		return err
	}
	defer func() { f.Close(); os.Remove(f.Name()) }()
	h := sha256.New()
	n, err := io.Copy(io.MultiWriter(f, h), io.LimitReader(r.Body, g.Package.Size+1))
	if err != nil {
		return err
	}
	if n != g.Package.Size || !strings.EqualFold(hex.EncodeToString(h.Sum(nil)), g.Package.SHA256) {
		return errors.New("package checksum/size mismatch; download discarded")
	}
	if err = f.Sync(); err != nil {
		return err
	}
	if err = f.Close(); err != nil {
		return err
	}
	// Link publishes the complete file atomically and refuses to overwrite an
	// existing destination (also safe against a concurrent downloader).
	if err = os.Link(f.Name(), output); err != nil {
		return fmt.Errorf("publish verified download (destination must not exist, filesystem must support hard links): %w", err)
	}
	return nil
}

func run(args []string) error {
	if len(args) == 0 {
		return errors.New("usage: gamestop {list|show|verify|upload|download|visibility|delete|version} [flags]; GAMESTOP_TOKEN supplies bearer token")
	}
	// version 不取 --id、不需要 token、也不联网：它只回答"我手上这个是哪一版"。
	if args[0] == "version" {
		fmt.Printf("gamestop %s\n", buildVersion)
		return nil
	}
	fs := flag.NewFlagSet(args[0], flag.ContinueOnError)
	base := fs.String("server", "https://game-stop.giveme8.com", "HTTPS server origin")
	id := fs.String("id", "", "game ID")
	file := fs.String("file", "", "upload .game/.pck path, verify target, or download destination (must not exist)")
	meta := fs.String("metadata", "", "upload metadata JSON file for bare .pck uploads; .game carries its own game.json")
	visibility := fs.String("visibility", "private", "public or private")
	if err := fs.Parse(args[1:]); err != nil {
		return err
	}
	if fs.NArg() != 0 {
		return errors.New("unexpected positional arguments")
	}
	c, err := newClient(*base, os.Getenv("GAMESTOP_TOKEN"))
	if err != nil {
		return err
	}
	cmd := args[0]
	if cmd != "list" && !validID.MatchString(*id) {
		return errors.New("--id must match [a-z0-9][a-z0-9-]{0,63}")
	}
	if (cmd == "upload" || cmd == "visibility" || cmd == "delete") && c.token == "" {
		return errors.New("set GAMESTOP_TOKEN for write operations")
	}
	path := "/api/v1/games/" + *id
	var result []byte
	switch cmd {
	case "list":
		result, err = c.jsonRequest("GET", "/api/v1/games", nil)
	case "show":
		result, err = c.jsonRequest("GET", path, nil)
	case "verify":
		// 本机自检：发布前先跑一遍服务端会跑的校验，别等上传被拒才发现。
		if *file == "" {
			return errors.New("--file path required")
		}
		return verifyPackage(*file, *id)
	case "visibility":
		if *visibility != "private" && *visibility != "public" {
			return errors.New("visibility must be public or private")
		}
		result, err = c.jsonRequest("PATCH", path, strings.NewReader(`{"visibility":"`+*visibility+`"}`))
	case "delete":
		result, err = c.jsonRequest("DELETE", path, nil)
	case "download":
		if *file == "" {
			return errors.New("--file destination required")
		}
		err = c.download(*id, *file)
		if err == nil {
			fmt.Println("Verified download:", *file)
		}
	case "upload":
		if *file == "" {
			return errors.New("--file required")
		}
		isBundle := strings.ToLower(filepath.Ext(*file)) == ".game"
		// `.game` 的元数据在包里；再给一份 --metadata 就出现两个真源，本地直接拦住。
		if isBundle && *meta != "" {
			return errors.New(".game carries its own game.json; drop --metadata")
		}
		if !isBundle {
			if strings.ToLower(filepath.Ext(*file)) != ".pck" {
				return errors.New("only .game or .pck uploads supported")
			}
			if *meta == "" {
				return errors.New("--metadata metadata.json required for bare .pck uploads")
			}
		}
		metadata := []byte{}
		if !isBundle {
			mf, e := os.Open(*meta)
			if e != nil {
				return e
			}
			metadata, e = io.ReadAll(io.LimitReader(mf, 8193))
			mf.Close()
			if e != nil {
				return e
			}
			if len(metadata) > 8192 || !json.Valid(metadata) {
				return errors.New("metadata must be valid JSON at most 8192 bytes")
			}
		}
		f, e := os.Open(*file)
		if e != nil {
			return e
		}
		defer f.Close()
		info, e := f.Stat()
		if e != nil {
			return e
		}
		if !info.Mode().IsRegular() || info.Size() <= 0 || info.Size() > maxPackage {
			return errors.New("package must be a regular file between 1 byte and 512 MiB")
		}
		h := sha256.New()
		encoded := ""
		if !isBundle {
			encoded = base64.StdEncoding.EncodeToString(metadata)
		}
		r, e := c.request("PUT", path, io.TeeReader(f, h), encoded, info.Size())
		if e != nil {
			return e
		}
		defer r.Body.Close()
		result, err = io.ReadAll(io.LimitReader(r.Body, (4<<20)+1))
		if err != nil {
			return err
		}
		if len(result) > 4<<20 {
			return errors.New("response too large")
		}
		var g game
		if e = json.Unmarshal(result, &g); e != nil {
			return e
		}
		if g.ID != *id || g.Package.Size <= 0 || g.Package.SHA256 == "" {
			return errors.New("server upload receipt is incomplete; inspect server record before retrying")
		}
		// 对账要用"服务端收到的字节"：容器提交时 package.* 描述的是解出来的 pck，
		// 拿它跟本地容器比必然不等（这里踩过）。老服务端没有 received，退回比 package.*。
		gotSize, gotSHA := g.Received.Size, g.Received.SHA256
		if gotSHA == "" {
			gotSize, gotSHA = g.Package.Size, g.Package.SHA256
		}
		if gotSize != info.Size() || !strings.EqualFold(gotSHA, hex.EncodeToString(h.Sum(nil))) {
			return errors.New("server upload receipt differs from local package; inspect server record before retrying")
		}
	default:
		return errors.New("unknown command: " + cmd)
	}
	if err != nil {
		return err
	}
	if len(result) > 0 {
		fmt.Println(string(result))
	}
	return nil
}

func main() {
	if err := run(os.Args[1:]); err != nil {
		fmt.Fprintln(os.Stderr, "gamestop:", err)
		os.Exit(1)
	}
}
