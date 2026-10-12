package library

import (
	"archive/zip"
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/url"
	"os"
	"path"
	"strings"
)

// `.game` 容器（zip）的读取与校验。契约见 docs/api/game-bundle-format.md。
//
// 三条不可动摇的前提：
//  1. 容器只是**分发**形态。运行期仍然是 pck，绝不把容器交给引擎挂载
//     （zip 资源包在本项目实测会段错误，见 constraints 第 12.1 节）。
//  2. 元数据的唯一真源是包内的 game.json；平台侧属性（可见性/所有者/时间戳/体积/sha256）
//     一律由平台自己算，包里写了也不采信。
//  3. 解压是新的攻击面：先读中央目录做完全部静态检查，再决定要不要解压任何内容。

const (
	// 条目数上限：正常包只有个位数到几十个文件。
	MaxBundleEntries = 512
	// 解压后总量上限（容器本身沿用 MaxPackageBytes = 512 MiB）。
	MaxBundleUncompressed = 768 << 20
	// 单条压缩率上限：解压炸弹的典型特征是极高的压缩比。
	MaxBundleRatio = 200

	MaxCoverBytes     = 4 << 20
	MaxScreenshotByte = 8 << 20
	MaxScreenshots    = 8

	bundleMetaName = "game.json"
	// 入口默认位置，game.json 可以用 entry 覆盖。
	bundleDefaultEntry = "content/game.pck"

	// zip 通用标志位：bit0 = 加密。
	zipFlagEncrypted = 1 << 0
)

var errBundle = errors.New("invalid or unsafe game bundle")

// BundleSource 是游戏本体的出处。平台**只记录、不拉取**：让服务端去 clone 任意仓库
// 等于开一个 SSRF 与构建农场的口子（ADR-0011）。
type BundleSource struct {
	Repo    string `json:"repo,omitempty"`
	Ref     string `json:"ref,omitempty"`
	Commit  string `json:"commit,omitempty"`
	License string `json:"license,omitempty"`
}

type bundleAssets struct {
	Cover       string   `json:"cover"`
	Screenshots []string `json:"screenshots,omitempty"`
}

// BundleMeta 就是小游戏仓库里在用的那份 game.json（扩展 source/assets/entry）。
// 未知字段一律忽略：将来加字段时老服务端不至于直接拒收（向前兼容）。
// 导出是给发布工具用的：`gamestop verify` 要在本机跑同一套校验并打印人类可读的结果。
type BundleMeta struct {
	ID          string       `json:"id"`
	Title       string       `json:"title"`
	Subtitle    string       `json:"subtitle,omitempty"`
	Description string       `json:"description,omitempty"`
	Tags        []string     `json:"tags,omitempty"`
	Accent      string       `json:"accent,omitempty"`
	Engine      Engine       `json:"engine"`
	Entry       string       `json:"entry,omitempty"`
	Source      BundleSource `json:"source,omitempty"`
	Assets      bundleAssets `json:"assets"`
	Order       int          `json:"order,omitempty"`
	// 各模式的**显示名**。平台只认 mode 的 id（排行榜按它分榜），但 id 是给机器的
	// （easy / normal / blind…），直接印在网页上没人看得懂——游戏里显示的是"轻松/普通/盲找"。
	// 由发布方在容器里声明一次，平台照着渲染；没声明的 id 原样显示。
	Modes []BundleMode `json:"modes,omitempty"`
}

// BundleMode 是一个模式的 id 与它的显示名。
type BundleMode struct {
	ID    string `json:"id"`
	Label string `json:"label"`
}

// BundleAsset 是容器里的一张图，以及它的容器内路径。
type BundleAsset struct {
	Path string // 容器内路径
	Ext  string // 由魔数决定的扩展名（.png/.jpg/.webp），不是包内文件名的扩展名
	Max  int64
}

// BundlePlan 是通过全部静态检查之后的结果：知道要解压哪些条目、元数据是什么。
type BundlePlan struct {
	Meta   BundleMeta
	Entry  string
	Assets []BundleAsset
	files  map[string]*zip.File
}

// LooksLikeBundle 用魔数判别容器。调用方据此区分"裸 pck"与"`.game`"。
func LooksLikeBundle(head []byte) bool {
	return len(head) >= 4 && head[0] == 'P' && head[1] == 'K' && head[2] == 0x03 && head[3] == 0x04
}

// InspectBundle 只读中央目录，做完全部静态检查并返回解压计划——**不写入任何东西**。
// wantID 是上传路由里的 id，必须与包内 game.json 一致（防止包里写一个、路由里写另一个）。
func InspectBundle(r io.ReaderAt, size int64, wantID string) (*BundlePlan, error) {
	if size < 22 || size > MaxPackageBytes {
		return nil, errBundle
	}
	archive, err := zip.NewReader(r, size)
	if err != nil {
		return nil, errBundle
	}
	if len(archive.File) == 0 || len(archive.File) > MaxBundleEntries {
		return nil, errBundle
	}
	plan := &BundlePlan{files: make(map[string]*zip.File)}
	var uncompressed uint64
	for _, f := range archive.File {
		if f.FileInfo().IsDir() {
			// 目录条目本身无害，但名字仍要合法，且不进文件表。
			if !safePath(strings.TrimSuffix(f.Name, "/")) {
				return nil, errBundle
			}
			continue
		}
		if !safePath(f.Name) {
			return nil, errBundle
		}
		if f.Flags&zipFlagEncrypted != 0 {
			return nil, errBundle
		}
		if f.Mode()&os.ModeSymlink != 0 {
			return nil, errBundle
		}
		// 解压炸弹：总量与单条压缩率都在中央目录里声明，先算完再决定解不解压。
		if f.CompressedSize64 == 0 && f.UncompressedSize64 > 0 {
			return nil, errBundle
		}
		if f.CompressedSize64 > 0 && f.UncompressedSize64/f.CompressedSize64 > MaxBundleRatio {
			return nil, errBundle
		}
		uncompressed += f.UncompressedSize64
		if uncompressed > MaxBundleUncompressed {
			return nil, errBundle
		}
		plan.files[f.Name] = f
	}

	metaFile, ok := plan.files[bundleMetaName]
	if !ok {
		return nil, errBundle
	}
	raw, err := readZipEntry(metaFile, 64<<10)
	if err != nil {
		return nil, err
	}
	var meta BundleMeta
	if err := json.Unmarshal(raw, &meta); err != nil {
		return nil, errBundle
	}
	if meta.ID != wantID || !idPattern.MatchString(meta.ID) {
		return nil, errBundle
	}
	// 复用既有的元数据约束（长度、标签、accent、engine）。
	if err := (Metadata{
		Title: meta.Title, Subtitle: meta.Subtitle, Description: meta.Description,
		Tags: meta.Tags, Accent: meta.Accent, Engine: meta.Engine,
	}).Validate(); err != nil {
		return nil, errBundle
	}
	if err := validateBundleSource(meta.Source); err != nil {
		return nil, err
	}
	if err := validateBundleModes(meta.Modes); err != nil {
		return nil, err
	}

	plan.Meta = meta
	plan.Entry = meta.Entry
	if plan.Entry == "" {
		plan.Entry = bundleDefaultEntry
	}
	if path.Ext(plan.Entry) != ".pck" {
		return nil, errBundle
	}
	if _, ok := plan.files[plan.Entry]; !ok {
		return nil, errBundle
	}

	// 封面必需；截图可选。
	cover, err := plan.asset(meta.Assets.Cover, MaxCoverBytes)
	if err != nil {
		return nil, errBundle
	}
	plan.Assets = append(plan.Assets, *cover)
	if len(meta.Assets.Screenshots) > MaxScreenshots {
		return nil, errBundle
	}
	for _, name := range meta.Assets.Screenshots {
		asset, err := plan.asset(name, MaxScreenshotByte)
		if err != nil {
			return nil, errBundle
		}
		plan.Assets = append(plan.Assets, *asset)
	}
	return plan, nil
}

func (p *BundlePlan) asset(name string, limit int64) (*BundleAsset, error) {
	if name == "" || !safePath(name) {
		return nil, errBundle
	}
	f, ok := p.files[name]
	if !ok || f.UncompressedSize64 > uint64(limit) {
		return nil, errBundle
	}
	// 只读前 512 字节嗅探魔数。**不能**用 readZipEntry——它会先拒绝"声明大小超过上限"
	// 的条目，而真实封面有几百 KB，那样每一张真封面都会被误判为非法。
	head, err := readZipPrefix(f, 512)
	if err != nil {
		return nil, err
	}
	ext := imageExt(head)
	if ext == "" {
		return nil, errBundle
	}
	return &BundleAsset{Path: name, Ext: ext, Max: limit}, nil
}

// OpenEntry 打开入口 pck 的流：调用方边读边算哈希/边落盘，不整包进内存。
func (p *BundlePlan) OpenEntry() (io.ReadCloser, error) {
	f, ok := p.files[p.Entry]
	if !ok {
		return nil, errBundle
	}
	return f.Open()
}

// OpenAsset 打开一张素材。
func (p *BundlePlan) OpenAsset(a BundleAsset) (io.ReadCloser, error) {
	f, ok := p.files[a.Path]
	if !ok {
		return nil, errBundle
	}
	return f.Open()
}

// PlatformMetadata 把包内元数据转成平台既有的 Metadata。
// visibility 由平台侧决定，不从包里取（同一个包换个人上传就是另一个可见性）。
func (p *BundlePlan) PlatformMetadata() Metadata {
	return Metadata{
		Title: p.Meta.Title, Subtitle: p.Meta.Subtitle, Description: p.Meta.Description,
		Tags: p.Meta.Tags, Accent: p.Meta.Accent, Engine: p.Meta.Engine,
		Modes: p.Meta.Modes,
	}
}

// validateBundleModes 校验发布方声明的模式显示名。
// id 必须与排行榜使用的字符集一致（否则这个标签永远匹配不上任何成绩），
// 显示名要短、不为空——它是印在网页按钮上的。
func validateBundleModes(modes []BundleMode) error {
	if len(modes) > 24 {
		return errBundle
	}
	seen := map[string]bool{}
	for _, m := range modes {
		if !idPattern.MatchString(m.ID) || seen[m.ID] {
			return errBundle
		}
		seen[m.ID] = true
		if m.Label == "" || len([]rune(m.Label)) > 12 {
			return errBundle
		}
	}
	return nil
}

func validateBundleSource(s BundleSource) error {
	// 逐项校验"写了的东西"，不要因为 repo 为空就整体跳过——
	// 那样一个只有 commit 的畸形 source 会被静默接受。
	if len(s.Repo) > 512 || len(s.Ref) > 128 || len(s.License) > 64 {
		return errBundle
	}
	if s.Commit != "" && (len(s.Commit) != 40 || !isHex(s.Commit)) {
		return errBundle
	}
	if s.Repo == "" {
		return nil
	}
	u, err := url.Parse(s.Repo)
	if err != nil || (u.Scheme != "https" && u.Scheme != "http") || u.Host == "" {
		return errBundle
	}
	// 带凭据的仓库地址等于把秘密写进一个会公开分发的包里。
	if u.User != nil {
		return errBundle
	}
	return nil
}

// imageExt 按文件头魔数判断图片类型；不信扩展名。
func imageExt(b []byte) string {
	switch {
	case len(b) >= 8 && bytes.Equal(b[:8], []byte{0x89, 'P', 'N', 'G', 0x0d, 0x0a, 0x1a, 0x0a}):
		return ".png"
	case len(b) >= 3 && b[0] == 0xff && b[1] == 0xd8 && b[2] == 0xff:
		return ".jpg"
	case len(b) >= 12 && string(b[:4]) == "RIFF" && string(b[8:12]) == "WEBP":
		return ".webp"
	}
	return ""
}

// readZipEntry 只读前 limit 字节，用于校验而不解压整条。
func readZipEntry(f *zip.File, limit int64) ([]byte, error) {
	if f.UncompressedSize64 > uint64(limit) {
		return nil, errBundle
	}
	rc, err := f.Open()
	if err != nil {
		return nil, errBundle
	}
	defer rc.Close()
	b, err := io.ReadAll(io.LimitReader(rc, limit+1))
	if err != nil || int64(len(b)) > limit {
		return nil, errBundle
	}
	return b, nil
}

// readZipPrefix 最多读前 n 字节，不要求整条都很小（用于嗅探文件头）。
func readZipPrefix(f *zip.File, n int64) ([]byte, error) {
	rc, err := f.Open()
	if err != nil {
		return nil, errBundle
	}
	defer rc.Close()
	b, err := io.ReadAll(io.LimitReader(rc, n))
	if err != nil {
		return nil, errBundle
	}
	return b, nil
}

// CopyLimited 把 r 拷进 w，超过 limit 立即报错。
// 解压时这是第二道防线：中央目录里声明的大小可能撒谎，实际字节数必须再兜一次。
func CopyLimited(w io.Writer, r io.Reader, limit int64) (int64, error) {
	n, err := io.Copy(w, io.LimitReader(r, limit+1))
	if err != nil {
		return n, err
	}
	if n > limit {
		return n, fmt.Errorf("%w: entry exceeds %d bytes", errBundle, limit)
	}
	return n, nil
}
