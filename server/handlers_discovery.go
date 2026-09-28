package server

// 搜索 / 推荐 / 榜单 / 专辑 / 歌手 端点（对标 quaver_server/app.py 相应区段）。
import (
	"encoding/json"
	"fmt"
	"net/http"
	"regexp"
	"strconv"

	"github.com/team-quaver/typhoeus-go/qqmusic"
)

// wikiDescRe 提取歌手百科 XML 里 <desc><![CDATA[...]]></desc> 的内容。
var wikiDescRe = regexp.MustCompile(`(?s)<desc><!\[CDATA\[(.*?)\]\]></desc>`)

func wikiDesc(wiki string) string {
	m := wikiDescRe.FindStringSubmatch(wiki)
	if m == nil {
		return ""
	}
	return m[1]
}

// singerAreaLabels 地区枚举 id → 展示文案（0=港台 1=内地 2=日韩 3=欧美 5=其他）。
var singerAreaLabels = map[string]string{"0": "港台", "1": "内地", "2": "日韩", "3": "欧美", "5": "其他"}

func singerAreaLabel(raw string) string {
	if raw == "" {
		raw = "0"
	}
	if n, err := strconv.Atoi(raw); err == nil {
		raw = strconv.Itoa(n)
	}
	return singerAreaLabels[raw]
}

// noneOrZeroToEmpty 0/空 → 空串（对齐上游 NoneOrZeroToEmptyStr）。
func noneOrZeroToEmpty(s string) string {
	if s == "" || s == "0" {
		return ""
	}
	return s
}

// ===================== 搜索 =====================

func (a *App) handleSearchHotkey(w http.ResponseWriter, _ *http.Request) {
	raw, err := a.call(false, a.search.GetHotkey)
	if err != nil {
		writeError(w, err)
		return
	}
	writeOK(w, json.RawMessage(raw))
}

func (a *App) handleSearchComplete(w http.ResponseWriter, r *http.Request) {
	keyword := r.URL.Query().Get("keyword")
	if keyword == "" {
		writeErr(w, 422, -1, "缺少 keyword")
		return
	}
	raw, err := a.call(false, func() (json.RawMessage, error) { return a.search.Complete(keyword) })
	if err != nil {
		writeError(w, err)
		return
	}
	writeOK(w, json.RawMessage(raw))
}

func (a *App) handleSearch(w http.ResponseWriter, r *http.Request) {
	keyword := r.URL.Query().Get("keyword")
	if keyword == "" {
		writeErr(w, 422, -1, "缺少 keyword")
		return
	}
	typ := queryInt(r, "type", 0)
	page := queryInt(r, "page", 1)
	num := queryInt(r, "num", 20)
	raw, err := a.call(false, func() (json.RawMessage, error) {
		return a.search.ByType(keyword, typ, page, num)
	})
	if err != nil {
		writeError(w, err)
		return
	}
	writeOK(w, json.RawMessage(raw))
}

func (a *App) handleSearchGeneral(w http.ResponseWriter, r *http.Request) {
	keyword := r.URL.Query().Get("keyword")
	if keyword == "" {
		writeErr(w, 422, -1, "缺少 keyword")
		return
	}
	page := queryInt(r, "page", 1)
	num := queryInt(r, "num", 15)
	raw, err := a.call(false, func() (json.RawMessage, error) {
		return a.search.General(keyword, page, num)
	})
	if err != nil {
		writeError(w, err)
		return
	}
	writeOK(w, json.RawMessage(raw))
}

// ===================== 推荐 / 榜单 =====================

func (a *App) handleRecommendGuess(w http.ResponseWriter, r *http.Request) {
	// 猜你喜欢（无限电台那一路）：上游一次只给 5 首，多调几轮按 mid 去重。
	// ⚠️ 必须串行：并发同样的请求上游只放行一个，其余全回 700000。
	// 单轮失败不算整体失败（保底返回已拿到的），全失败才抛。
	rounds := queryInt(r, "rounds", 6)
	if rounds < 1 {
		rounds = 1
	} else if rounds > 8 {
		rounds = 8
	}
	songs := make([]json.RawMessage, 0, rounds*5)
	seen := map[string]bool{}
	for i := 0; i < rounds; i++ {
		raw, err := a.call(false, a.recom.GuessRecommend)
		if err != nil {
			// 单轮失败（含 700000 节流）不该拖垮整页
			logWarn("猜你喜欢第 %d/%d 轮失败：%v", i+1, rounds, err)
			continue
		}
		var resp struct {
			Tracks []json.RawMessage `json:"tracks"`
		}
		if json.Unmarshal(raw, &resp) != nil {
			continue
		}
		for _, track := range resp.Tracks {
			var t struct {
				Mid string `json:"mid"`
			}
			if json.Unmarshal(track, &t) != nil || t.Mid == "" || seen[t.Mid] {
				continue
			}
			seen[t.Mid] = true
			songs = append(songs, track)
		}
	}
	if len(songs) == 0 {
		writeErr(w, http.StatusBadGateway, -http.StatusBadGateway, "猜你喜欢上游无响应，请稍后重试")
		return
	}
	writeOK(w, map[string]any{"songs": songs, "rounds": rounds})
}

func (a *App) handleRecommendSonglist(w http.ResponseWriter, r *http.Request) {
	page := queryInt(r, "page", 1)
	num := queryInt(r, "num", 25)
	raw, err := a.call(false, func() (json.RawMessage, error) {
		return a.recom.GetRecommendSonglist(page, num)
	})
	if err != nil {
		writeError(w, err)
		return
	}
	writeOK(w, json.RawMessage(raw))
}

func (a *App) handleRecommendNewsong(w http.ResponseWriter, r *http.Request) {
	typ := queryInt(r, "type", 5)
	raw, err := a.call(false, func() (json.RawMessage, error) {
		return a.recom.GetRecommendNewsong(typ)
	})
	if err != nil {
		writeError(w, err)
		return
	}
	writeOK(w, json.RawMessage(raw))
}

// dailyDirid 「每日30首」= 系统虚拟歌单，dirid 固定 202（与「我喜欢」= 201 同一族）：
// 每天由服务端重生成一份 30 首的个性化歌单，disstid 每天都变——按 dirid 取才稳。
// 读法与普通歌单详情完全一致（CgiGetDiss），disstid 与 dirid 都传 202。
const dailyDirid int64 = 202

func (a *App) handleRecommendDaily(w http.ResponseWriter, r *http.Request) {
	page := queryInt(r, "page", 1)
	num := queryInt(r, "num", 50)
	// dirid=202（每日三十首）上游要求登录态（匿名请求报 CGI 10004），
	// 走 needLogin 让未登录请求拿到干净的 401，而不是透传上游裸错误。
	raw, err := a.call(true, func() (json.RawMessage, error) {
		return a.songlist.GetDetail(dailyDirid, dailyDirid, int64(page), int64(num), false)
	})
	if err != nil {
		writeError(w, err)
		return
	}
	writeOK(w, json.RawMessage(raw))
}

func (a *App) handleTopCategory(w http.ResponseWriter, _ *http.Request) {
	raw, err := a.call(false, a.top.GetCategory)
	if err != nil {
		writeError(w, err)
		return
	}
	writeOK(w, json.RawMessage(raw))
}

func (a *App) handleTopDetail(w http.ResponseWriter, r *http.Request) {
	var topID int64
	fmt.Sscanf(r.PathValue("top_id"), "%d", &topID)
	page := queryInt(r, "page", 1)
	num := queryInt(r, "num", 50)
	raw, err := a.call(false, func() (json.RawMessage, error) {
		return a.top.GetDetail(topID, int64(page), int64(num))
	})
	if err != nil {
		writeError(w, err)
		return
	}
	writeOK(w, json.RawMessage(raw))
}

// ===================== 专辑 / 歌手 =====================

func (a *App) handleAlbumDetail(w http.ResponseWriter, r *http.Request) {
	value := r.PathValue("value")
	raw, err := a.call(false, func() (json.RawMessage, error) {
		return a.album.GetDetail(value)
	})
	if err != nil {
		writeError(w, err)
		return
	}
	writeOK(w, json.RawMessage(raw))
}

func (a *App) handleAlbumSongs(w http.ResponseWriter, r *http.Request) {
	value := r.PathValue("value")
	page := queryInt(r, "page", 1)
	num := queryInt(r, "num", 50)
	raw, err := a.call(false, func() (json.RawMessage, error) {
		return a.album.GetSongs(value, page, num)
	})
	if err != nil {
		writeError(w, err)
		return
	}
	writeOK(w, json.RawMessage(raw))
}

func (a *App) handleSingerInfo(w http.ResponseWriter, r *http.Request) {
	mid := r.PathValue("mid")
	raw, err := a.call(false, func() (json.RawMessage, error) {
		return a.singer.GetInfo(mid)
	})
	if err != nil {
		writeError(w, err)
		return
	}
	writeOK(w, json.RawMessage(raw))
}

func (a *App) handleSingerSongs(w http.ResponseWriter, r *http.Request) {
	mid := r.PathValue("mid")
	page := queryInt(r, "page", 1)
	num := queryInt(r, "num", 50)
	order := queryInt(r, "order", 1)
	// order 必须显式透传：1=按热度（热门）、2=按发行时间倒序（最新发布）。
	// 漏传时两次请求拿到同一批热门歌，前端去重后新歌面板恒为空。
	raw, err := a.call(false, func() (json.RawMessage, error) {
		return a.singer.GetSongsList(mid, page, num, order)
	})
	if err != nil {
		writeError(w, err)
		return
	}
	writeOK(w, json.RawMessage(raw))
}

func (a *App) handleSingerAlbums(w http.ResponseWriter, r *http.Request) {
	mid := r.PathValue("mid")
	page := queryInt(r, "page", 1)
	num := queryInt(r, "num", 30)
	raw, err := a.call(false, func() (json.RawMessage, error) {
		return a.singer.GetAlbumList(mid, page, num, 1)
	})
	if err != nil {
		writeError(w, err)
		return
	}
	writeOK(w, json.RawMessage(raw))
}

func (a *App) handleSingerSimilar(w http.ResponseWriter, r *http.Request) {
	mid := r.PathValue("mid")
	number := queryInt(r, "number", 10)
	raw, err := a.call(false, func() (json.RawMessage, error) {
		return a.singer.GetSimilar(mid, number)
	})
	if err != nil {
		writeError(w, err)
		return
	}
	writeOK(w, json.RawMessage(raw))
}

func (a *App) handleSingerDesc(w http.ResponseWriter, r *http.Request) {
	mid := r.PathValue("mid")
	raw, err := a.call(false, func() (json.RawMessage, error) {
		return a.singer.GetDesc([]string{mid})
	})
	if err != nil {
		writeError(w, err)
		return
	}
	payload, err := extractSingerDesc(raw)
	if err != nil {
		writeError(w, err)
		return
	}
	writeOK(w, payload)
}

// extractSingerDesc 歌手详情归一化：百科简介（wiki <desc> 优先，回退 ex_info.desc）
// + 外文名/生日/地区/立绘。
func extractSingerDesc(raw json.RawMessage) (any, error) {
	var resp struct {
		SingerList []struct {
			BasicInfo struct {
				Name string `json:"name"`
				Mid  string `json:"singer_mid"`
			} `json:"basic_info"`
			Pic struct {
				Pic      string `json:"pic"`
				BigBlack string `json:"big_black"`
			} `json:"pic"`
			ExInfo struct {
				Desc        string      `json:"desc"`
				ForeignName string      `json:"foreign_name"`
				Birthday    string      `json:"birthday"`
				Area        json.Number `json:"area"`
				Identity    json.Number `json:"identity"`
			} `json:"ex_info"`
			Wiki string `json:"wiki"`
		} `json:"singer_list"`
	}
	if err := json.Unmarshal(raw, &resp); err != nil {
		return nil, qqmusic.NewDataError("歌手详情解析失败")
	}
	if len(resp.SingerList) == 0 {
		return nil, nil
	}
	s := resp.SingerList[0]
	desc := wikiDesc(s.Wiki)
	if desc == "" {
		desc = s.ExInfo.Desc
	}
	return map[string]any{
		"name":         s.BasicInfo.Name,
		"mid":          s.BasicInfo.Mid,
		"pic":          s.Pic.Pic,
		"big_pic":      s.Pic.BigBlack,
		"desc":         desc,
		"foreign_name": s.ExInfo.ForeignName,
		"birthday":     s.ExInfo.Birthday,
		// area 经规整：港台歌手上游给 0，输出「港台」；未知值给空串（不漏裸数字）
		"area": singerAreaLabel(s.ExInfo.Area.String()),
		// identity 经规整：0（无身份）→ 空串，与上游 NoneOrZeroToEmptyStr 一致
		"identity": noneOrZeroToEmpty(s.ExInfo.Identity.String()),
	}, nil
}
