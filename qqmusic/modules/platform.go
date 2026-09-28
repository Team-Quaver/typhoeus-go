package modules

import "github.com/team-quaver/typhoeus-go/qqmusic"

// NormalizePlatform 把请求级平台字符串归一为 qqmusic.Platform；
// 空串/未知值返回 ""（= CgiCall 缺省 ANDROID，保持既有语义）。
func NormalizePlatform(s string) qqmusic.Platform {
	switch s {
	case "web":
		return qqmusic.PlatformWeb
	case "desktop":
		return qqmusic.PlatformDesktop
	case "android":
		return qqmusic.PlatformAndroid
	default:
		return ""
	}
}
