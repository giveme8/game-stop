package library

import (
	"bytes"
	"context"
	_ "embed"
	"encoding/json"
	"errors"
	"fmt"
	"html/template"
	"net/http"
	"sort"
	"time"

	"gorm.io/gorm"
)

//go:embed leaderboard.html
var leaderboardHTML string

var leaderboardTemplate = template.Must(template.New("leaderboard").Funcs(template.FuncMap{
	"result": leaderboardResult,
	// 时间按**北京时间**渲染。原先印 UTC，玩家 20:13 打完看到 12:13:53，
	// 第一反应是"这时间不对"。网站刻意不用 JS，所以固定用 +08:00 并标明。
	"date":     func(t time.Time) string { return t.In(beijing).Format("2006-01-02 15:04") },
	"datetime": func(t time.Time) string { return t.UTC().Format(time.RFC3339) },
}).Parse(leaderboardHTML))

// beijing 是排行榜展示用的时区。中国用户看的是它，服务器跑在 UTC。
var beijing = time.FixedZone("CST", 8*60*60)

type leaderboardPageData struct {
	ID, Title, Board string
	Mode             string
	Modes            []string
	// Boards 是**真的有成绩**的榜；分数榜/用时榜不再是写死在模板里的按钮。
	Boards []string
	// ModeLabel 把模式 id 翻成发布方声明的显示名（没有声明就用 id 本身）。
	ModeLabel  string
	ModeLabels map[string]string
	// ModeOrder 是发布方声明的先后顺序（就是游戏里的难度梯度：轻松→普通→…）；
	// 没声明的模式排在后面。按中文标签排是没意义的——那顺序对用户是随机的。
	ModeOrder    map[string]int
	Entries      []LeaderboardEntry
	Unavailable  bool
	NotFound     bool
	InvalidBoard bool
}

// ModeName 把模式 id 翻成显示名：发布方在容器里声明过就用它，没声明就原样显示 id。
// 这样新游戏/新难度不用改平台代码，老记录也不会因为缺声明而崩。
// 导出是因为模板要调用它（Go 模板看不到未导出方法）。
func (p leaderboardPageData) ModeName(mode string) string {
	if label, ok := p.ModeLabels[mode]; ok && label != "" {
		return label
	}
	return mode
}

// BoardName 是榜单的中文名。两个榜的语义不同（一个越高越好、一个越低越好），
// 所以名字里带方向箭头——玩家不用猜"分数"和"用时"哪个大才好。
func (p leaderboardPageData) BoardName(board string) string {
	if board == "time-ms" {
		return "用时榜 ↓"
	}
	return "分数榜 ↑"
}

func leaderboardResult(value int64, board string) string {
	if board == "time-ms" {
		return fmt.Sprintf("%d.%03d 秒", value/1000, value%1000)
	}
	return fmt.Sprintf("%d 分", value)
}

func (s *Server) leaderboardPage(w http.ResponseWriter, r *http.Request) {
	page := leaderboardPageData{Board: "score", Mode: normalizedMode(r.URL.Query().Get("mode"))}
	status := http.StatusOK
	id := r.PathValue("id")
	var game Game
	// Never use readable here: even authenticated owners must not expose a
	// private game's metadata or scores on a public leaderboard page.
	if !idPattern.MatchString(id) {
		page.NotFound, status = true, http.StatusNotFound
	} else {
		err := s.DB.WithContext(r.Context()).Where("id = ? AND visibility = ? AND deleted = ?", id, "public", false).First(&game).Error
		switch {
		case errors.Is(err, gorm.ErrRecordNotFound):
			page.NotFound, status = true, http.StatusNotFound
		case err != nil:
			page.Unavailable, status = true, http.StatusServiceUnavailable
		default:
			var metadata Metadata
			if err := json.Unmarshal([]byte(game.Metadata), &metadata); err != nil {
				page.Unavailable, status = true, http.StatusServiceUnavailable
				break
			}
			page.ID, page.Title = game.ID, metadata.Title
			query := r.URL.Query()
			if board := query.Get("board"); board != "" {
				page.Board = board
			}
			// 没显式指定时，默认落在**成绩最多的那个榜**上——board 与 mode 一起定。
			//
			// 两个写死值都踩过：mode 写死 "default"（没人写的遗留值，游戏用的是
			// easy/normal/hard/insane/blind/zen），board 写死 "score"（而找数字与接龙
			// 提交的是 time-ms）。任一不匹配，刚打完一局的玩家打开页面就是空榜，
			// 结论自然变成"成绩没上去"——而成绩其实早就提交成功了。
			explicitBoard := query.Get("board") != ""
			explicitMode := len(query["mode"]) > 0
			// 先把**请求本身**校验掉，再碰数据库：畸形的 board/mode 是客户端错误（400），
			// 不能因为数据库正好也不可用就变成 503——那样调用方会以为是服务端故障。
			if !idPattern.MatchString(page.Mode) || len(query["mode"]) > 1 || (page.Board != "score" && page.Board != "time-ms") {
				page.Board = "score"
				page.InvalidBoard, status = true, http.StatusBadRequest
				break
			}
			// 一次查出"哪些 (榜, 模式) 真的有成绩"，三件事都用它：
			// 挑默认值、决定榜单选择器、决定难度选择器。
			type scoredPair struct {
				Board string
				Mode  string
				N     int64
			}
			var pairs []scoredPair
			if e := s.DB.WithContext(r.Context()).Model(&PlayerScore{}).
				Where("game_id = ?", game.ID).Select("board, mode, COUNT(*) AS n").
				Group("board, mode").Order("n DESC, MAX(achieved_at) DESC").Scan(&pairs).Error; e != nil {
				page.Unavailable, status = true, http.StatusServiceUnavailable
				break
			}
			if !explicitBoard || !explicitMode {
				if len(pairs) > 0 {
					top := pairs[0]
					if !explicitBoard && validBoard(top.Board) {
						page.Board = top.Board
					}
					if !explicitMode && idPattern.MatchString(top.Mode) {
						page.Mode = top.Mode
					}
				}
			}
			publicRequest := r.WithContext(context.WithValue(r.Context(), identityKey{}, uint64(0)))
			// 只列**真的有成绩**的榜与模式。
			//
			// 以前无条件塞一个 "default" 模式，而"分数榜"更是写死在模板里的：找数字只提交
			// 用时（它是计时游戏），那个"分数榜"永远点开是空的——和死模式一样，是"看不懂"
			// 的来源。显式指定的那个下面会单独补进来，所以不会丢。
			page.Modes = nil
			page.Boards = nil
			seenBoard := map[string]bool{}
			for _, p := range pairs {
				if idPattern.MatchString(p.Mode) && !containsMode(page.Modes, p.Mode) {
					page.Modes = append(page.Modes, p.Mode)
				}
				if validBoard(p.Board) && !seenBoard[p.Board] {
					seenBoard[p.Board] = true
					page.Boards = append(page.Boards, p.Board)
				}
			}
			page.ModeLabels = map[string]string{}
			page.ModeOrder = map[string]int{}
			for i, m := range metadata.Modes {
				if m.Label != "" {
					page.ModeLabels[m.ID] = m.Label
				}
				page.ModeOrder[m.ID] = i
			}
			// 只有**显式指定**的模式才补进选择器：一个合格的 mode 即使还没人打过也是合法的
			// （游戏里的"排行榜"按钮总是传当前难度，第一次点进来应该看到空榜而不是 400）。
			// 但**推导出来的**默认模式不补——否则"没有任何成绩"的游戏会显示一个孤零零的
			// "default" 按钮，点开还是空榜，这正是"看不懂"的来源。
			if explicitMode && !containsMode(page.Modes, page.Mode) {
				page.Modes = append(page.Modes, page.Mode)
			}
			// 显式指定了一个还没成绩的榜时同理：保证它出现在选择器里（能打开空榜）。
			if explicitBoard && !containsMode(page.Boards, page.Board) {
				page.Boards = append(page.Boards, page.Board)
			}
			// 排序按发布方声明的顺序（游戏里的难度梯度），没声明的按 id 排在后面。
			// 按显示名排没意义：中文标签的字典序对用户是随机的。
			sort.SliceStable(page.Modes, func(i, j int) bool {
				oi, iOK := page.ModeOrder[page.Modes[i]]
				oj, jOK := page.ModeOrder[page.Modes[j]]
				switch {
				case iOK && jOK:
					return oi < oj
				case iOK != jOK:
					return iOK
				default:
					return page.Modes[i] < page.Modes[j]
				}
			})
			page.ModeLabel = page.ModeName(page.Mode)
			page.Entries, err = s.leaderboardEntries(publicRequest, game.ID, page.Board, page.Mode, 100)
			if errors.Is(err, errMissing) {
				page = leaderboardPageData{Board: "score", NotFound: true}
				status = http.StatusNotFound
			} else if err != nil {
				page.Entries = nil
				page.Unavailable, status = true, http.StatusServiceUnavailable
			}
		}
	}
	var body bytes.Buffer
	if err := leaderboardTemplate.Execute(&body, page); err != nil {
		fail(w, http.StatusServiceUnavailable, "page unavailable")
		return
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Header().Set("Cache-Control", "private, no-store")
	w.Header().Set("Content-Security-Policy", "default-src 'none'; style-src 'unsafe-inline'; base-uri 'none'; frame-ancestors 'none'; form-action 'none'")
	w.Header().Set("Referrer-Policy", "no-referrer")
	w.WriteHeader(status)
	if r.Method != http.MethodHead {
		_, _ = w.Write(body.Bytes())
	}
}

func containsMode(modes []string, mode string) bool {
	for _, candidate := range modes {
		if candidate == mode {
			return true
		}
	}
	return false
}
