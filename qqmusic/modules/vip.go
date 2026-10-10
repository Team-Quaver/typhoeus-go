package modules

import (
	"encoding/json"
	"strconv"
	"strings"
	"time"

	"github.com/team-quaver/typhoeus-go/qqmusic"
)

// VipInfo 是 Quaver 的稳定会员契约。svip 在此明确指「超级会员」，而不是
// 旧 vip_login_base 中同名的「豪华绿钻」。三个权益的时间段不可互相借用。
type VipInfo struct {
	Svip         int64       `json:"svip"`
	SvipStart    string      `json:"svip_start"`
	SvipEnd      string      `json:"svip_end"`
	SvipYearFlag int64       `json:"svip_year_flag"`
	Identity     VipIdentity `json:"identity"`
}

type VipIdentity struct {
	Vip          int64  `json:"vip"`
	VipStart     string `json:"vip_start"`
	VipEnd       string `json:"vip_end"`
	HugeVip      int64  `json:"huge_vip"`
	HugeVipStart string `json:"huge_vip_start"`
	HugeVipEnd   string `json:"huge_vip_end"`
	YearFlag     int64  `json:"year_flag"`
	HugeYearFlag int64  `json:"huge_year_flag"`
	Level        int64  `json:"level"`
}

// parseVipQuery 对齐官方 myservice 页：HugeVip = 超会，iSuperVip = 绿豪，
// iVipFlag = 绿钻；superStart/EndTime 属于绿豪，HugeVipStart/End 属于超会。
func parseVipQuery(data json.RawMessage, uin int64) (VipInfo, error) {
	var response struct {
		InfoMap map[string]map[string]any `json:"infoMap"`
	}
	if err := json.Unmarshal(data, &response); err != nil {
		return VipInfo{}, qqmusic.NewDataError("会员权益响应格式异常")
	}
	v := response.InfoMap[strconv.FormatInt(uin, 10)]
	if len(v) == 0 || (v["HugeVip"] == nil && v["iSuperVip"] == nil && v["iVipFlag"] == nil) {
		return VipInfo{}, qqmusic.NewDataError("会员权益响应缺少当前账号")
	}
	return VipInfo{
		Svip:      jsonInt(v, "HugeVip"),
		SvipStart: jsonStr(v, "HugeVipStart"), SvipEnd: jsonStr(v, "HugeVipEnd"),
		SvipYearFlag: jsonInt(v, "HugeYearFlag"),
		Identity: VipIdentity{
			Vip:      jsonInt(v, "iVipFlag"),
			VipStart: jsonStr(v, "sStartDateTime"), VipEnd: jsonStr(v, "sOverDateTime"),
			HugeVip:      jsonInt(v, "iSuperVip"),
			HugeVipStart: jsonStr(v, "superStartTime"), HugeVipEnd: jsonStr(v, "superEndTime"),
			YearFlag: jsonInt(v, "iYearFlag"), HugeYearFlag: jsonInt(v, "iYearFlag"),
			Level: jsonInt(v, "iCurLevel"),
		},
	}, nil
}

// parseVipLogin 旧 CGI 的顶层 svip = 绿豪，identity.HugeVip = 超会。
// 只有本权益明确给出的时间才能搬运；缺失的绿豪时间不借用超会 HugeVipEnd。
func parseVipLogin(data json.RawMessage) (VipInfo, error) {
	var v map[string]any
	if err := json.Unmarshal(data, &v); err != nil {
		return VipInfo{}, qqmusic.NewDataError("登录权益响应格式异常")
	}
	id := jsonObj(v, "identity")
	if v["svip"] == nil && id["vip"] == nil && id["HugeVip"] == nil && id["huge_vip"] == nil {
		return VipInfo{}, qqmusic.NewDataError("登录权益响应缺少会员字段")
	}
	// snake_case 是旧 SDK dump 形态，CamelCase 是 CGI 原始形态；存在值优先，包括 0。
	first := func(src map[string]any, keys ...string) any {
		for _, k := range keys {
			if val, ok := src[k]; ok && val != nil {
				return val
			}
		}
		return nil
	}
	number := func(src map[string]any, keys ...string) int64 {
		return jsonInt(map[string]any{"v": first(src, keys...)}, "v")
	}
	text := func(src map[string]any, keys ...string) string { s, _ := first(src, keys...).(string); return s }
	return VipInfo{
		Svip:      number(id, "huge_vip", "HugeVip"),
		SvipStart: text(id, "huge_vip_start", "HugeVipStart"), SvipEnd: text(id, "huge_vip_end", "HugeVipEnd"),
		SvipYearFlag: number(id, "huge_year_flag", "HugeYearFlag"),
		Identity: VipIdentity{
			HugeVip: number(v, "svip"), HugeVipStart: text(v, "svip_start", "superStartTime"), HugeVipEnd: text(v, "svip_end", "superEndTime"),
			Vip: number(id, "vip"), VipStart: text(id, "vip_start", "sStartDateTime"), VipEnd: text(id, "vip_end", "sOverDateTime"),
			YearFlag: number(id, "year_flag", "yearflag"), HugeYearFlag: number(id, "year_flag", "yearflag"), Level: number(id, "level"),
		},
	}, nil
}

var vipZone = time.FixedZone("Asia/Shanghai", 8*60*60)

// ParseVipTime 严格解析上游北京时间墙钟。日期型到期值有效到该日末尾。
func ParseVipTime(raw string, end bool) (time.Time, bool) {
	s := strings.ReplaceAll(strings.TrimSpace(raw), "T", " ")
	for _, layout := range []string{"2006-01-02 15:04:05", "2006-01-02 15:04", "2006-01-02"} {
		t, err := time.ParseInLocation(layout, s, vipZone)
		if err != nil || t.Year() < 2000 || t.Year() > 2100 {
			continue
		}
		if end && layout == "2006-01-02" {
			t = t.AddDate(0, 0, 1).Add(-time.Second)
		}
		return t, true
	}
	return time.Time{}, false
}

// VipActive 不凭时间记录臆造会员。显式无效日期按无效处理；未返回时间时才信标志。
func VipActive(flag int64, start, end string, now time.Time) bool {
	if flag != 1 {
		return false
	}
	for _, bound := range []struct {
		raw string
		end bool
	}{{start, false}, {end, true}} {
		if strings.TrimSpace(bound.raw) == "" || strings.TrimSpace(bound.raw) == "0" {
			continue
		}
		t, ok := ParseVipTime(bound.raw, bound.end)
		if !ok || (!bound.end && now.Before(t)) || (bound.end && !now.Before(t)) {
			return false
		}
	}
	return true
}

// Active 返回当前有效权益；保留独立起止时间供界面显示历史记录。
func (v VipInfo) Active(now time.Time) VipInfo {
	if !VipActive(v.Svip, v.SvipStart, v.SvipEnd, now) {
		v.Svip = 0
	}
	if !VipActive(v.Identity.HugeVip, v.Identity.HugeVipStart, v.Identity.HugeVipEnd, now) {
		v.Identity.HugeVip = 0
	}
	if !VipActive(v.Identity.Vip, v.Identity.VipStart, v.Identity.VipEnd, now) {
		v.Identity.Vip = 0
	}
	return v
}
