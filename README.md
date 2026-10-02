# Typhoeus Server (Go)

Quaver Typhoeus 的 Golang 服务后端。替代 L-1124 版 Python API 服务端与 Typhoeus 1.0 后端

## 功能

- **登录**：QQ 二维码 / 微信二维码 / QQ 音乐手机客户端扫码（MQTT 5.0 over WSS 推送）+ 凭证自动刷新 + 登出
- **账号**：主页信息、VIP 会员信息、我喜欢（红心列表）、自建歌单、收藏歌单
- **播放流（Typhoeus）**：
  - 明文高阶档位全开：标准 128 → HQ → SQ(FLAC/OGG 640) → **臻品音质 2.0 / 全景声 5.1 / 7.1 / 母带 / DTS:X / 杜比全景声(AC-4)**
  - **加密档位解密播放**（黑胶/加密 FLAC/加密 OGG 等）：**全链路不落盘、无整文件内存峰值、ekey 不透传**
  - 会员门控（403）+ rank 回退链 + 自动降档模式（`auto`）+ 回退链降权（`deprioritize`）
  - 首块嗅探防「明文档位偶发下发密文」；加密流解密后嗅探验证 ekey 正确性
  - 标准 Range 中继（206/416/后缀区间），断流自动按已发字节重连续传（2 次）
- **歌单**：详情、加歌/删歌（写侧 songType）、红心收藏/取消、歌单收藏/取消
- **每日三十首**：系统虚拟歌单 `dirid=202`（每天由服务端重新生成）
- **无限电台**：猜你喜欢多轮串行拉取 + 按 mid 去重（上游单次只给 5 首）
- **其余**：搜索（类型/综合/补全/热搜）、榜单、专辑、歌手、歌词、热评
- **系统集成**： MPRIS/NowPlaying/SMTC、Inhibit/电源断言、XDG 桌面门户与全局快捷键。

## 运行

```bash
go build -o typhoeus-go ./cmd/quaver-server
QUAVER_PORT=3200 ./typhoeus-go        # 默认 http://127.0.0.1:3200
```

| 环境变量 | 说明 |
|---|---|
| `QUAVER_PORT` | 监听端口（默认 3200） |
| `QUAVER_CONFIG_DIR` | 配置根目录（Electron 主进程注入） |
| `QUAVER_CREDENTIAL_MODE=external` | 凭证经 stdin/stdout 交接给主进程加密落盘；未设 = memory 模式（登录只驻内存） |
| `QSG_DEBUG_CGI=1` | （诊断用）向 stderr 打印每条 CGI 请求体——**含凭证，勿在日志中留存** |

**凭证交接**（与 ui/electron/keyring.mjs 的 `QCRED1 ` 前缀逐字兼容）：
- 启动时主进程往 stdin 写一行 `QCRED1 {json}`（或 `QCRED1 null`）；
- 登录/刷新/登出时本进程往 stdout 回写 `QCRED1 {json|null}`，由主进程加密落盘。
- memory 模式绝不读写凭证明文——宁可不持久化。

设备指纹 `device.json`（非密钥）与缓存 `device.cache.json`（QIMEI/匿名会话）存于配置目录。

## HTTP API

统一信封：成功 `{code:0, msg:"ok", data:…}`；失败 `{code:-1|-<status>, msg:…}` + 对应 HTTP 状态码
（401 凭证无效 / 403 会员门控 / 404 流过期 / 422 参数 / 429 风控 / 451 加密档拒绝 / 502 上游）。

| 方法 | 路径 | 说明 |
|---|---|---|
| GET  | `/login/qrcode/{qq\|wx\|mobile}` | 取登录二维码（data=base64 图，img=dataURL） |
| GET  | `/login/qrcode/{type}/status?identifier=` | 轮询状态（mobile 型走 MQTT 后台推送） |
| GET  | `/login/status` / `/login/refresh` | 登录态 / 手动刷新凭证 |
| POST | `/login/logout` | 登出 |
| GET  | `/user/me` `/user/vip` `/user/liked` `/user/created-songlists` `/user/fav-songlists` | 账号信息 |
| GET  | `/stream/tiers` | 会员等级 + 可播/全量档位表（含 `encrypted` 标记） |
| POST | `/stream/resolve` | `{mid, media_mid?, tier, auto?, deprioritize?}` → 换取中继 token |
| GET  | `/stream/{token}` | Range 透传中继（`<audio>` 的 src 指这里） |
| POST | `/song/urls` | 批量取链（file_type 序号表见 `server/handlers_content.go`） |
| GET  | `/song/{id\|mid}/detail` `/song/{v}/url` `/song/{v}/lyric` `/song/{id}/comments` | 歌曲域 |
| POST | `/song/like` `/song/unlike` | 红心（`{song_id, song_type}`，**写侧枚举**=读侧-1） |
| GET  | `/songlist/{id}/detail` | 歌单详情（每日30首走 `/recommend/daily`） |
| POST/DELETE | `/songlist/{id}/like` | 歌单收藏/取消 |
| POST/DELETE | `/songlist/{dirid}/songs` | 加歌/删歌（`{song_id, song_type, tid?}`） |
| GET  | `/songlist/fav/check` | 已收藏歌单 ID 集合 |
| GET  | `/search` `/search/general` `/search/complete` `/search/hotkey` | 搜索域 |
| GET  | `/recommend/guess` `/recommend/songlist` `/recommend/newsong` `/recommend/daily` | 推荐域 |
| GET  | `/top/category` `/top/{id}/detail` | 榜单 |
| GET  | `/album/{id\|mid}/detail` `/album/{v}/songs` | 专辑 |
| GET  | `/singer/{mid}/info` `/songs?order=` `/albums` `/similar` `/desc` | 歌手 |

档位 id：`128` `320` `320ogg` `640ogg` `flac` `atmos2` `atmos51` `atmos71` `master` `dts` `atmosdb` `vinyl`。
带 `encrypted: true` 的响应表示该流由后端不落盘解密（仅会员）。

## 已知边界

- 匿名搜索（`/search`）可能触发上游风控（429），登录后正常——与上游客户端行为一致。
- NAC（TL01，腾讯自研 codec）浏览器不可播，未进档位表。
- 手机验证码登录（SMS）上游支持但未在本服务暴露，需要时可加。
