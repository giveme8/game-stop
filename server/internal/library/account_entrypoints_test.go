package library

import (
	"net/http/httptest"
	"strings"
	"testing"
)

// 注册/登录入口必须能从公开页面点得到。
//
// 此前 /register、/login、/account 三个路由都在，但首页和排行榜页上**一个链接都没有**：
// 功能上线了玩家也发现不了，只能手打网址。这条测试就是防止再漏。
func TestPublicPagesLinkToAccountEntryPoints(t *testing.T) {
	db := testDB(t)
	if err := db.Create(&Game{
		ID: "entry-game", OwnerID: 1, Visibility: "public",
		Metadata: `{"title":"入口游戏","engine":{"name":"godot","version":"4.7"}}`,
		Filename: "f.pck",
	}).Error; err != nil {
		t.Fatal(err)
	}
	s, err := NewServer(db, t.TempDir(), "https://games.example")
	if err != nil {
		t.Fatal(err)
	}
	for _, path := range []string{"/", "/leaderboards/entry-game?board=score"} {
		w := httptest.NewRecorder()
		s.Handler().ServeHTTP(w, httptest.NewRequest("GET", path, nil))
		if w.Code != 200 {
			t.Fatalf("%s status %d", path, w.Code)
		}
		body := w.Body.String()
		for _, link := range []string{`href="/login"`, `href="/register"`, `href="/account"`} {
			if !strings.Contains(body, link) {
				t.Errorf("%s 没有指向 %s 的链接", path, link)
			}
		}
		// 首页还要把"怎么才能上榜"讲清楚，否则链接点了也不知道下一步做什么。
		if path == "/" {
			for _, text := range []string{"需要管理员审核", "签发", "玩家账号"} {
				if !strings.Contains(body, text) {
					t.Errorf("首页缺少上榜说明 %q", text)
				}
			}
		}
	}
}
