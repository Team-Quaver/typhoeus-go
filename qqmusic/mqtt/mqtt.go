// Package mqtt 实现本项目用到的 MQTT 5.0 over WSS 最小客户端子集。
//
// 只覆盖 QQ 音乐「手机客户端扫码登录」推送通道（mu.y.qq.com:443/wss/handshake）
// 实际会用到的报文：CONNECT / CONNACK / SUBSCRIBE / SUBACK / PUBLISH / PINGREQ /
// PINGRESP / DISCONNECT，以及 MQTT 5.0 属性。协议语义与 paho-mqtt 的线上字节
// 逐帧对拍验证（CONNECT 属性区带长度前缀、SUBSCRIBE 属性区在变长头内）。
package mqtt

import (
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"strconv"
	"sync"
	"time"

	"github.com/gorilla/websocket"
)

// MQTT 5.0 报文类型（fixed header 高 4 位）。
const (
	typeConnect    = 1
	typeConnack    = 2
	typePublish    = 3
	typeSubscribe  = 8
	typeSuback     = 9
	typePingreq    = 12
	typePingresp   = 13
	typeDisconnect = 14
	flagSubscribe  = 0x02 // SUBSCRIBE 固定头标志位
)

// 属性 ID（本项目关心的）。
const (
	propServerRef    = 0x1C
	propUserProperty = 0x26
)

// CONNACK 原因码。
const (
	reasonSuccess         = 0x00
	reasonServerMoved     = 0x9C // 临时重定向（携带 ServerReference）
	reasonServerMovedPerm = 0x9D // 永久重定向
)

// Property 用户属性键值对。
type Property struct{ K, V string }

// Message 下行 PUBLISH 消息（QoS0）。
type Message struct {
	Topic      string
	Payload    []byte
	Properties map[string]string // UserProperty 键值对
}

// Client MQTT 5.0 over WSS 客户端。
//
// 生命周期：Connect → Subscribe → 循环 ReadMessage → Close。
// KeepAlive 由内部 goroutine 自动维护；断线后 ReadMessage 返回错误。
type Client struct {
	clientID string
	host     string
	port     int
	path     string
	headers  map[string]string

	ws      *websocket.Conn
	wsMu    sync.Mutex
	pktID   uint16
	pktMu   sync.Mutex
	closed  bool
	closeMu sync.Mutex

	goodPath string // 本次会话成功使用的握手路径（Connect 成功后可读）

	// 单读循环 + 通道分发：gorilla WebSocket 不允许并发读
	msgCh  chan *Message
	subChs chan subAck
	subErr chan error
}

type subAck struct {
	pktID uint16
	codes []byte
}

// NewClient 构造客户端（未连接）。
func NewClient(clientID, host string, port int, path string) *Client {
	return &Client{
		clientID: clientID,
		host:     host,
		port:     port,
		path:     path,
		msgCh:    make(chan *Message, 64),
		subChs:   make(chan subAck, 8),
		subErr:   make(chan error, 1),
	}
}

// ---- 编码 ----

func writeUTFString(s string) []byte {
	out := make([]byte, 2+len(s))
	binary.BigEndian.PutUint16(out, uint16(len(s)))
	copy(out[2:], s)
	return out
}

// writeVarint MQTT 变长整数编码（每字节 7 位，最高位续位）。
func writeVarint(n int) []byte {
	var out []byte
	for {
		d := byte(n % 128)
		n /= 128
		if n > 0 {
			d |= 0x80
		}
		out = append(out, d)
		if n == 0 {
			return out
		}
	}
}

// readVarint MQTT 变长整数解码，返回值与新偏移。
func readVarint(pkt []byte, pos int) (int, int, bool) {
	mul, val := 1, 0
	for {
		if pos >= len(pkt) {
			return 0, pos, false
		}
		b := pkt[pos]
		pos++
		val += int(b&0x7F) * mul
		if b&0x80 == 0 {
			return val, pos, true
		}
		mul *= 128
	}
}

// encodeRemaining 构造 fixed header + remaining length + body。
func encodeRemaining(packetType, flags byte, body []byte) []byte {
	out := append([]byte{(packetType << 4) | flags}, writeVarint(len(body))...)
	return append(out, body...)
}

// encodeProps 编码属性区内容（不含长度前缀）：authMethod + UserProperty 序列。
func encodeProps(authMethod string, userProps []Property) []byte {
	var b []byte
	if authMethod != "" {
		b = append(b, 0x15) // AuthenticationMethod（UTF 字符串）
		b = append(b, writeUTFString(authMethod)...)
	}
	for _, p := range userProps {
		b = append(b, 0x26) // UserProperty
		b = append(b, writeUTFString(p.K)...)
		b = append(b, writeUTFString(p.V)...)
	}
	return b
}

// ---- 连接 ----

// Connect 建立 WSS 连接并完成 CONNECT/CONNACK 握手。
// 服务端重定向（0x9C/0x9D + ServerReference）自动跟随（最多 3 次）。
// initialPath 非空时作为起始握手路径（用于命中上次成功的节点，省一次重定向往返）。
func (c *Client) Connect(ctx context.Context, authMethod string, userProps []Property, headers map[string]string, initialPath string) error {
	c.headers = headers
	connectProps := encodeProps(authMethod, userProps)
	currentPath := c.path
	if initialPath != "" {
		currentPath = initialPath
	}
	var lastErr error
	for attempt := 0; attempt < 4; attempt++ {
		reason, serverRef, err := c.connectOnce(ctx, currentPath, connectProps)
		if err != nil {
			lastErr = err
			break
		}
		if reason == reasonSuccess {
			c.path = currentPath
			c.goodPath = currentPath
			go c.keepAliveLoop()
			go c.readLoop()
			return nil
		}
		if (reason == reasonServerMoved || reason == reasonServerMovedPerm) && serverRef != "" {
			c.closeWS()
			currentPath = redirectPath(currentPath, serverRef)
			lastErr = fmt.Errorf("mqtt: 服务端重定向到 %s", serverRef)
			continue
		}
		c.closeWS()
		return fmt.Errorf("mqtt: connect 被拒绝 reason=0x%02X%s", reason, orEmpty(serverRef))
	}
	c.closeWS()
	if lastErr == nil {
		lastErr = errors.New("mqtt: connect 失败")
	}
	return lastErr
}

func orEmpty(s string) string {
	if s == "" {
		return ""
	}
	return " (" + s + ")"
}

// redirectPath 对齐上游 _build_redirect_path：
// 末段已是节点名（含端口冒号）则整段替换，否则在路径后追加新节点。
func redirectPath(path, serverRef string) string {
	if i := lastIndexByte(path, ':'); i > lastIndexByte(path, '/') {
		return path[:i] + serverRef
	}
	if lastIndexByte(path, '/') == len(path)-1 {
		return path + serverRef
	}
	return path + "/" + serverRef
}

func lastIndexByte(s string, b byte) int {
	for i := len(s) - 1; i >= 0; i-- {
		if s[i] == b {
			return i
		}
	}
	return -1
}

// connectOnce 单次 WSS 连接 + CONNECT 报文，返回 CONNACK 原因码与 ServerReference。
func (c *Client) connectOnce(ctx context.Context, path string, connectProps []byte) (byte, string, error) {
	dialer := websocket.Dialer{HandshakeTimeout: 20 * time.Second}
	u := url.URL{Scheme: "wss", Host: fmt.Sprintf("%s:%d", c.host, c.port), Path: path}
	hdr := http.Header{}
	for k, v := range c.headers {
		hdr.Set(k, v)
	}
	hdr.Set("Sec-WebSocket-Protocol", "mqtt") // paho 同款子协议头，缺了会被节点掐断
	ws, resp, err := dialer.DialContext(ctx, u.String(), hdr)
	if err != nil {
		if resp != nil {
			return 0, "", fmt.Errorf("mqtt: ws 建连失败: %v (HTTP %d)", err, resp.StatusCode)
		}
		return 0, "", fmt.Errorf("mqtt: ws 建连失败: %w", err)
	}
	c.wsMu.Lock()
	c.ws = ws
	c.wsMu.Unlock()

	// CONNECT：协议名/版本/flags/keepalive + 属性区(带 varint 长度前缀!) + clientID
	body := []byte{}
	body = append(body, 0x00, 0x04)
	body = append(body, 'M', 'Q', 'T', 'T')
	body = append(body, 5)          // protocol version 5
	body = append(body, 0x02)       // clean start
	body = append(body, 0x00, 0x2D) // keep alive 45s
	body = append(body, writeVarint(len(connectProps))...)
	body = append(body, connectProps...)
	body = append(body, writeUTFString(c.clientID)...)
	if err := c.writePacket(encodeRemaining(typeConnect, 0, body)); err != nil {
		return 0, "", err
	}

	_ = ws.SetReadDeadline(time.Now().Add(20 * time.Second))
	pkt, err := readOnePacket(ws)
	if err != nil {
		return 0, "", fmt.Errorf("mqtt: 等待 connack 失败: %w", err)
	}
	if len(pkt) == 0 || pkt[0]>>4 != typeConnack {
		return 0, "", errors.New("mqtt: 未收到 CONNACK")
	}
	_ = ws.SetReadDeadline(time.Time{})
	reason, serverRef := parseConnack(pkt)
	return reason, serverRef, nil
}

// parseConnack 解析 CONNACK：fixed header → [flags] [reasonCode] [属性区]。
func parseConnack(pkt []byte) (byte, string) {
	if len(pkt) < 4 {
		return 0xFF, ""
	}
	pos := 1
	_, next, ok := readVarint(pkt, pos)
	if !ok {
		return 0xFF, ""
	}
	pos = next
	if pos+2 > len(pkt) {
		return 0xFF, ""
	}
	reason := pkt[pos+1]
	props := pkt[pos+2:]
	serverRef := ""
	if len(props) > 0 {
		propsLen, consumed, ok := readVarint(props, 0)
		if ok && consumed+propsLen <= len(props) {
			parsed := parseProps(props[consumed : consumed+propsLen])
			serverRef = parsed[strconv.Itoa(propServerRef)]
		}
	}
	return reason, serverRef
}

// parseProps 解析 MQTT 5.0 属性区（按各属性的真实线上格式推进）。
// UserProperty 存为 "user:<键名>"；其余属性存为十进制 ID 字符串键。
func parseProps(body []byte) map[string]string {
	out := map[string]string{}
	for i := 0; i < len(body); {
		id := int(body[i])
		i++
		switch id {
		case 0x01, 0x17, 0x19, 0x24, 0x25, 0x28, 0x29, 0x2A: // 单字节
			if i+1 > len(body) {
				return out
			}
			out[strconv.Itoa(id)] = strconv.Itoa(int(body[i]))
			i++
		case 0x02, 0x11, 0x18, 0x27: // u32
			if i+4 > len(body) {
				return out
			}
			out[strconv.Itoa(id)] = strconv.FormatUint(uint64(binary.BigEndian.Uint32(body[i:])), 10)
			i += 4
		case 0x13, 0x21, 0x22, 0x23: // u16
			if i+2 > len(body) {
				return out
			}
			out[strconv.Itoa(id)] = strconv.FormatUint(uint64(binary.BigEndian.Uint16(body[i:])), 10)
			i += 2
		case 0x0B: // varint
			v, next, ok := readVarint(body, i)
			if !ok {
				return out
			}
			out[strconv.Itoa(id)] = strconv.Itoa(v)
			i = next
		case 0x03, 0x08, 0x12, 0x15, 0x1A, 0x1C, 0x1F: // UTF 字符串
			if i+2 > len(body) {
				return out
			}
			n := int(binary.BigEndian.Uint16(body[i:]))
			i += 2
			if i+n > len(body) {
				return out
			}
			out[strconv.Itoa(id)] = string(body[i : i+n])
			i += n
		case 0x09, 0x16: // 二进制（u16 长度前缀）
			if i+2 > len(body) {
				return out
			}
			n := int(binary.BigEndian.Uint16(body[i:]))
			i += 2 + n
			if i > len(body) {
				return out
			}
		case propUserProperty: // UTF 键值对
			if i+2 > len(body) {
				return out
			}
			n := int(binary.BigEndian.Uint16(body[i:]))
			i += 2
			if i+n > len(body) {
				return out
			}
			k := string(body[i : i+n])
			i += n
			if i+2 > len(body) {
				return out
			}
			n = int(binary.BigEndian.Uint16(body[i:]))
			i += 2
			if i+n > len(body) {
				return out
			}
			out["user:"+k] = string(body[i : i+n])
			i += n
		default: // 未知属性：无法安全跳过，终止解析
			return out
		}
	}
	return out
}

// ---- 收发 ----

// writePacket 写一帧（gorilla 写互斥）。
func (c *Client) writePacket(pkt []byte) error {
	c.wsMu.Lock()
	defer c.wsMu.Unlock()
	if c.ws == nil {
		return errors.New("mqtt: 连接未建立")
	}
	return c.ws.WriteMessage(websocket.BinaryMessage, pkt)
}

// readOnePacket 从指定连接读一帧。
func readOnePacket(ws *websocket.Conn) ([]byte, error) {
	_, data, err := ws.ReadMessage()
	if err != nil {
		return nil, err
	}
	return data, nil
}

// readLoop 唯一的下行读取 goroutine：
// PINGRESP 内部消化、SUBACK 投递给等待者、PUBLISH 推入消息通道；断线即收尾。
func (c *Client) readLoop() {
	defer close(c.msgCh)
	for {
		c.wsMu.Lock()
		ws := c.ws
		c.wsMu.Unlock()
		if ws == nil {
			c.failSubs(errors.New("mqtt: 连接已断开"))
			return
		}
		pkt, err := readOnePacket(ws)
		if err != nil {
			c.failSubs(errors.New("mqtt: 连接已断开"))
			return
		}
		if len(pkt) == 0 {
			continue
		}
		switch pkt[0] >> 4 {
		case typePingresp:
			continue
		case typeSuback:
			if id, codes, ok := parseSuback(pkt); ok {
				select {
				case c.subChs <- subAck{pktID: id, codes: codes}:
				default:
				}
			}
		case typePublish:
			if msg := parsePublish(pkt); msg != nil {
				select {
				case c.msgCh <- msg:
				default: // 队列满则丢弃（登录通道消息量极小）
				}
			}
		}
	}
}

func (c *Client) failSubs(err error) {
	select {
	case c.subErr <- err:
	default:
	}
}

// parseSuback 解析 SUBACK：packetID(2) + 属性区(varint 长度) + 原因码列表。
func parseSuback(pkt []byte) (uint16, []byte, bool) {
	if len(pkt) < 5 {
		return 0, nil, false
	}
	id := binary.BigEndian.Uint16(pkt[2:4])
	// readVarint 返回绝对新位置：consumed 即属性区起点
	propsLen, consumed, ok := readVarint(pkt, 4)
	if !ok || consumed+propsLen > len(pkt) {
		return 0, nil, false
	}
	return id, pkt[consumed+propsLen:], true
}

// parsePublish 解析 PUBLISH（QoS0）：topic + 属性区 + payload。
func parsePublish(pkt []byte) *Message {
	if len(pkt) < 2 || pkt[0]&0x0F != 0x00 {
		return nil // 只处理 QoS0 推送
	}
	pos := 1
	_, next, ok := readVarint(pkt, pos)
	if !ok || next+2 > len(pkt) {
		return nil
	}
	pos = next
	topicLen := int(binary.BigEndian.Uint16(pkt[pos : pos+2]))
	pos += 2
	if pos+topicLen > len(pkt) {
		return nil
	}
	topic := string(pkt[pos : pos+topicLen])
	pos += topicLen
	// QoS0 无 packet id；紧跟属性区
	propsLen, propsStart, ok := readVarint(pkt, pos)
	if !ok {
		return nil
	}
	propsEnd := propsStart + propsLen
	if propsEnd > len(pkt) {
		return nil
	}
	return &Message{
		Topic:      topic,
		Payload:    pkt[propsEnd:],
		Properties: parseProps(pkt[pos:propsEnd]),
	}
}

// Subscribe 发送 SUBSCRIBE 并等待 SUBACK。
// 属性区在变长头内（packetID 之后）；载荷只有 topic + 订阅选项。
func (c *Client) Subscribe(topic string, userProps []Property) error {
	c.pktMu.Lock()
	c.pktID++
	id := c.pktID
	c.pktMu.Unlock()

	props := encodeProps("", userProps)
	vh := []byte{byte(id >> 8), byte(id)}
	vh = append(vh, writeVarint(len(props))...)
	vh = append(vh, props...)
	body := writeUTFString(topic)
	body = append(body, 0x00) // 订阅选项：QoS0
	if err := c.writePacket(encodeRemaining(typeSubscribe, flagSubscribe, append(vh, body...))); err != nil {
		return err
	}
	deadline := time.After(45 * time.Second)
	for {
		select {
		case ack := <-c.subChs:
			if ack.pktID != id {
				continue // 其他订阅的确认（本客户端串行订阅，实际不会发生）
			}
			for _, code := range ack.codes {
				if code >= 0x80 {
					return fmt.Errorf("mqtt: suback 拒绝 reason=0x%02X", code)
				}
			}
			return nil
		case err := <-c.subErr:
			return err
		case <-deadline:
			return errors.New("mqtt: subscribe 超时")
		}
	}
}

// GoodPath 返回本次成功使用的握手路径（空 = 未成功连接过）。
func (c *Client) GoodPath() string { return c.goodPath }

// ReadMessage 阻塞读取下一条 PUBLISH 消息。
func (c *Client) ReadMessage() (*Message, error) {
	msg, ok := <-c.msgCh
	if !ok {
		return nil, errors.New("mqtt: 连接已断开")
	}
	return msg, nil
}

// keepAliveLoop 按 40s 间隔发 PINGREQ（低于协商的 45s，留余量）。
func (c *Client) keepAliveLoop() {
	ticker := time.NewTicker(40 * time.Second)
	defer ticker.Stop()
	for range ticker.C {
		c.closeMu.Lock()
		if c.closed {
			c.closeMu.Unlock()
			return
		}
		c.closeMu.Unlock()
		if err := c.writePacket([]byte{(typePingreq << 4), 0}); err != nil {
			return
		}
	}
}

// Close 断开连接。
func (c *Client) Close() {
	c.closeMu.Lock()
	if c.closed {
		c.closeMu.Unlock()
		return
	}
	c.closed = true
	c.closeMu.Unlock()
	_ = c.writePacket([]byte{(typeDisconnect << 4), 0x00}) // normal disconnection
	c.closeWS()
}

func (c *Client) closeWS() {
	c.wsMu.Lock()
	defer c.wsMu.Unlock()
	if c.ws != nil {
		_ = c.ws.Close()
		c.ws = nil
	}
}
