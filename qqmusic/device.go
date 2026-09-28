package qqmusic

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"math/big"
	"os"
	"path/filepath"
)

// Device 虚拟 Android 设备指纹（对标 utils/device.py Device）。
//
// 它是「设备身份」而不是密钥：与客户端配置同目录明文保存 device.json，
// 跨重启保持同一身份，避免每次启动都换设备触发风控。
type Device struct {
	Display       string `json:"display"`
	Product       string `json:"product"`
	DeviceName    string `json:"device"`
	Board         string `json:"board"`
	Model         string `json:"model"`
	Fingerprint   string `json:"fingerprint"`
	BootID        string `json:"boot_id"`
	ProcVersion   string `json:"proc_version"`
	IMEI          string `json:"imei"`
	Brand         string `json:"brand"`
	Bootloader    string `json:"bootloader"`
	BaseBand      string `json:"base_band"`
	Version       OSVer  `json:"version"`
	SimInfo       string `json:"sim_info"`
	OSType        string `json:"os_type"`
	MacAddress    string `json:"mac_address"`
	WifiBSSID     string `json:"wifi_bssid"`
	WifiSSID      string `json:"wifi_ssid"`
	ImsiMD5       []int  `json:"imsi_md5"`
	AndroidID     string `json:"android_id"`
	APN           string `json:"apn"`
	VendorName    string `json:"vendor_name"`
	VendorOSName  string `json:"vendor_os_name"`
	OpenUDID      string `json:"open_udid"`
	OpenUDID2     string `json:"open_udid2"`
	Manufacturer  string `json:"manufacturer"`
	Host          string `json:"host"`
	FirstAPILevel int    `json:"first_api_level"`
}

// OSVer 系统版本信息。
type OSVer struct {
	Incremental string `json:"incremental"`
	Release     string `json:"release"`
	Codename    string `json:"codename"`
	SDK         int    `json:"sdk"`
}

// deviceProfile 一组内部一致的 Android 设备属性（与上游同款机型数据）。
type deviceProfile struct {
	display, product, device, board, model, fingerprint, procVersion string
	brand, manufacturer, host                                        string
	firstAPILevel                                                    int
	version                                                          OSVer
	vendorName, vendorOSName                                         string
}

var deviceProfiles = []deviceProfile{
	{
		display: "PD2408D_A_16.1.18.2.W10", product: "PD2408", device: "PD2408", board: "sun", model: "V2408A",
		fingerprint: "vivo/PD2408/PD2408:15/AP3A.240905.015.A2/compiler250423182036:user/release-keys",
		procVersion: "Linux localhost 6.6.89-android15-8-g1f71897ac249-abogki467805059-4k " +
			"#1 SMP PREEMPT Thu Dec 11 01:56:00 UTC 2025 aarch64",
		brand: "vivo", manufacturer: "vivo", host: "comdg01150014", firstAPILevel: 35,
		version:      OSVer{Incremental: "compiler250423182036", Release: "15", Codename: "REL", SDK: 35},
		vendorName:   "OriginOS",
		vendorOSName: "OriginOS 5.0",
	},
	{
		display: "OS3.0.260511.1.WOCCNXM.STABLE-OS31", product: "dada", device: "dada", board: "sun", model: "24129PN74C",
		fingerprint: "Xiaomi/dada/dada:16/BP2A.250605.031.A3/OS3.0.260511.1.WOCCNXM.STABLE-OS31:user/release-keys",
		brand:       "Xiaomi", manufacturer: "Xiaomi", host: "", firstAPILevel: 35,
		version:      OSVer{Incremental: "OS3.0.260511.1.WOCCNXM.STABLE-OS31", Release: "16", Codename: "REL", SDK: 36},
		vendorName:   "Xiaomi",
		vendorOSName: "HyperOS 3.0",
	},
	{
		display: "V.1ab312a_1-2a261", product: "PKB110", device: "OP5A3DL1", board: "mt6991", model: "PKB110",
		fingerprint: "OPPO/PKB110/OP5A3DL1:15/AP3A.240617.008/V.1ab312a_1-2a261:user/release-keys",
		brand:       "OPPO", manufacturer: "OPPO", host: "", firstAPILevel: 35,
		version:      OSVer{Incremental: "V.1ab312a_1-2a261", Release: "15", Codename: "REL", SDK: 35},
		vendorName:   "ColorOS",
		vendorOSName: "ColorOS 15",
	},
}

// randomHex 生成 n 字节的随机 hex 字符串（2n 长度）。
func randomHex(n int) string {
	b := make([]byte, n)
	_, _ = rand.Read(b)
	return hex.EncodeToString(b)
}

// randomUUIDv4 随机 UUID。
func randomUUIDv4() string {
	b := make([]byte, 16)
	_, _ = rand.Read(b)
	b[6] = (b[6] & 0x0F) | 0x40
	b[8] = (b[8] & 0x3F) | 0x80
	h := hex.EncodeToString(b)
	return h[0:8] + "-" + h[8:12] + "-" + h[12:16] + "-" + h[16:20] + "-" + h[20:32]
}

// randomIMEI 生成满足 Luhn 校验的随机 IMEI。
func randomIMEI() string {
	digits := make([]int, 14)
	sum := 0
	for i := range digits {
		n, _ := rand.Int(rand.Reader, big.NewInt(10))
		digits[i] = int(n.Int64())
		d := digits[i]
		if i%2 == 1 {
			d *= 2
			if d > 9 {
				d -= 9
			}
		}
		sum += d
	}
	ctrl := (10 - sum%10) % 10
	out := make([]byte, 0, 15)
	for _, d := range digits {
		out = append(out, byte('0'+d))
	}
	return string(out) + string(rune('0'+ctrl))
}

// randomLocalMAC 生成「本地管理位=1、单播」的随机 MAC（冒号分隔，与上游一致）。
func randomLocalMAC() string {
	b := make([]byte, 5)
	_, _ = rand.Read(b)
	out := "02"
	for _, x := range b {
		out += ":" + hexByte2(x)
	}
	return out
}

func hexByte2(b byte) string {
	const hexd = "0123456789ABCDEF"
	return string([]byte{hexd[b>>4], hexd[b&0xF]})
}

// GenerateDevice 随机生成一台内部一致的虚拟设备。
func GenerateDevice() *Device {
	p := deviceProfiles[randomIntN(int64(len(deviceProfiles)))]
	imsi := make([]int, 16)
	for i := range imsi {
		b := make([]byte, 1)
		_, _ = rand.Read(b)
		imsi[i] = int(b[0])
	}
	androidID := randomHex(8)
	return &Device{
		Display: p.display, Product: p.product, DeviceName: p.device, Board: p.board,
		Model: p.model, Fingerprint: p.fingerprint,
		BootID: randomUUIDv4(), ProcVersion: p.procVersion,
		IMEI: randomIMEI(), Brand: p.brand, Bootloader: "U-boot", BaseBand: "",
		Version: p.version, SimInfo: "T-Mobile", OSType: "android",
		MacAddress: randomLocalMAC(), WifiBSSID: randomLocalMAC(), WifiSSID: "<unknown ssid>",
		ImsiMD5: imsi, AndroidID: androidID, APN: "wifi",
		VendorName: p.vendorName, VendorOSName: p.vendorOSName,
		OpenUDID: randomHex(16), OpenUDID2: randomHex(16),
		Manufacturer: p.manufacturer, Host: p.host, FirstAPILevel: p.firstAPILevel,
	}
}

// randomIntN [0, n) 随机数。
func randomIntN(n int64) int64 {
	v, err := rand.Int(rand.Reader, big.NewInt(n))
	if err != nil {
		return 0
	}
	return v.Int64()
}

// randomString 从字母表随机取 n 个字符。
func randomString(alphabet string, n int) string {
	out := make([]byte, n)
	for i := range out {
		out[i] = alphabet[randomIntN(int64(len(alphabet)))]
	}
	return string(out)
}

// loadOrInitDevice 从 path 加载设备指纹；文件缺失/损坏时生成新设备并落盘。
// 字段级合并：旧文件缺字段（例如未来新增字段）时用新生成的值补齐。
func loadOrInitDevice(path string) (*Device, error) {
	if path == "" {
		return GenerateDevice(), nil
	}
	raw, err := os.ReadFile(path)
	if err == nil && len(raw) > 0 {
		dev := GenerateDevice()
		if err := json.Unmarshal(raw, dev); err == nil {
			return dev, nil
		}
		// 文件存在但解析失败：按新设备处理并覆盖
	}
	dev := GenerateDevice()
	if err := writeDevice(dev, path); err != nil {
		return dev, err
	}
	return dev, nil
}

// writeDevice 保存设备指纹（0600 权限；目录已由调用方建好）。
func writeDevice(dev *Device, path string) error {
	raw, err := json.MarshalIndent(dev, "", " ")
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return err
	}
	return os.WriteFile(path, raw, 0o600)
}
