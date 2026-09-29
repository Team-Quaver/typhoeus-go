#!/usr/bin/env python3
# -*- coding: utf-8 -*-
"""QMC 解密参考实现（纯 Python，unlock-music lib_um_crypto_rust 对拍移植）。

Go 侧 qmc 包的跨语言对拍锚点：tools/gen_qmc_vectors.py 用本文件构造
随机主密钥的 EKey + 密文片段，Go TestCrossVectors 逐项验证全链路一致。
本文件刻意独立实现（不 import 任何 Go 侧共享代码），对拍才有意义。

覆盖：tc_tea CBC（加密）、EKey V1/V2 构造、QMCv2 Map（≤300 字节主密钥）、
QMCv2 RC4（>300 字节主密钥）。f32 语义用 struct 往返模拟（ekey simple key
依赖它，值错一位 EKey 就解不开 —— 这正是对拍要抓的）。
"""
import base64
import math
import struct

EKEY_V2_PREFIX = base64.b64encode(b"QQMusic EncV2,Key:")

# tc_tea V2 双层固定密钥（对应 Rust ekey.rs KEY1/KEY2，字符串字面量）
EKEY_V2_KEY1 = b"386ZJY!@#*$%^&)("
EKEY_V2_KEY2 = b"**#!(#$%&^a1cZ,T"

TEA_DELTA = 0x9E3779B9
TEA_ROUNDS = 16


def _f32(x: float) -> float:
    return struct.unpack("f", struct.pack("f", x))[0]


def _u32(x: int) -> int:
    return x & 0xFFFFFFFF


def _tea_ecb(block: int, k) -> int:
    y, z = block >> 32, block & 0xFFFFFFFF
    s = 0
    for _ in range(TEA_ROUNDS):
        s = _u32(s + TEA_DELTA)
        y = _u32(y + (((z << 4) + k[0]) ^ (z + s) ^ ((z >> 5) + k[1])))
        z = _u32(z + (((y << 4) + k[2]) ^ (y + s) ^ ((y >> 5) + k[3])))
    return (y << 32) | z


def _tea_ecb_dec(block: int, k) -> int:
    y, z = block >> 32, block & 0xFFFFFFFF
    s = _u32(TEA_DELTA * TEA_ROUNDS)
    for _ in range(TEA_ROUNDS):
        z = _u32(z - (((y << 4) + k[2]) ^ (y + s) ^ ((y >> 5) + k[3])))
        y = _u32(y - (((z << 4) + k[0]) ^ (z + s) ^ ((z >> 5) + k[1])))
        s = _u32(s - TEA_DELTA)
    return (y << 32) | z


def _key_words(key16: bytes):
    return [int.from_bytes(key16[i * 4:i * 4 + 4], "big") for i in range(4)]


def tea_cbc_encrypt(plain: bytes, key16: bytes, salt: bytes) -> bytes:
    """tc_tea 变体 CBC 加密：1 字节 pad 长度 + pad + 2 字节 salt | 明文 | 7 字节零尾。"""
    k = _key_words(key16)
    out_len = 10 + len(plain)
    pad_len = (8 - out_len & 7) & 7
    header_len = 1 + pad_len + 2
    out_len += pad_len

    header = bytearray(16)
    header[:2] = salt
    header[0] = header[0] & ~7 | pad_len
    copy_len = min(16 - header_len, len(plain))
    header[header_len:header_len + copy_len] = plain[:copy_len]
    rest = plain[copy_len:]

    iv1 = iv2 = 0
    out = bytearray()
    def enc(block8: bytes) -> bytes:
        nonlocal iv1, iv2
        b = int.from_bytes(block8, "big")
        iv2_next = b ^ iv1
        c = _tea_ecb(iv2_next, k) ^ iv2
        iv1, iv2 = c, iv2_next
        return c.to_bytes(8, "big")

    out += enc(header[0:8])
    out += enc(header[8:16])
    while len(rest) >= 8:
        out += enc(rest[:8])
        rest = rest[8:]
    if rest:
        out += enc(rest + b"\x00" * (8 - len(rest)))
    return bytes(out[:out_len])


def tea_cbc_decrypt(cipher: bytes, key16: bytes) -> bytes:
    k = _key_words(key16)
    assert len(cipher) % 8 == 0 and len(cipher) >= 10
    plain = bytearray()
    iv1 = iv2 = 0
    for i in range(0, len(cipher), 8):
        block = int.from_bytes(cipher[i:i + 8], "big")
        iv2_next = _tea_ecb_dec(block ^ iv2, k)
        plain += (iv2_next ^ iv1).to_bytes(8, "big")
        iv1, iv2 = block, iv2_next
    pad = plain[0] & 7
    start = 1 + pad + 2
    end = len(cipher) - 7
    return bytes(plain[start:end])


def ekey_simple_key() -> bytes:
    """Rust make_simple_key::<8>()：tan(106 + i*0.1) 绝对值 *100，f32 语义全程。"""
    out = []
    f01 = _f32(0.1)
    for i in range(8):
        v = _f32(_f32(106.0) + _f32(_f32(i) * f01))
        t = _f32(abs(math.tan(v)))
        v = _f32(t * _f32(100.0))
        out.append(min(255, max(0, int(v))))
    return bytes(out)


EKEY_SIMPLE_KEY = ekey_simple_key()  # 与 Go makeSimpleKey / Rust 同源，对拍锚点


def build_v1_ekey(master: bytes, salt: bytes) -> str:
    header, body = master[:8], master[8:]
    tea_key = bytearray()
    for sk, hk in zip(ekey_simple_key(), header):
        tea_key += bytes([sk, hk])
    cipher = tea_cbc_encrypt(body, bytes(tea_key), salt)
    return base64.b64encode(header + cipher).decode()


def build_v2_ekey(master: bytes, salt1: bytes, salt2: bytes, salt3: bytes, zeros: int) -> str:
    v1_text = build_v1_ekey(master, salt1).encode()
    layer2 = tea_cbc_encrypt(bytes(v1_text) + b"\x00" * zeros, EKEY_V2_KEY2, salt2)
    layer1 = tea_cbc_encrypt(layer2, EKEY_V2_KEY1, salt3)
    return EKEY_V2_PREFIX.decode() + base64.b64encode(layer1).decode()


class QMC2Map:
    """QMCv2 短密钥（≤300 字节）静态映射流密码。

    主密钥先压缩成 128 字节映射表（(i²+71214)%n 取位 + 半字节旋转），
    字节索引按绝对偏移折叠：>0x7FFF 先 %0x7FFF，再 %128 —— 索引 0x7FFF→127、
    0x8000→1，周期在 0x7FFF 处跳变一次，是逆向出的原始行为。
    """

    def __init__(self, master: bytes):
        n = len(master)
        key = bytearray(128)
        for i in range(128):
            idx = (i * i + 71214) % n
            v = master[idx]
            shift = (idx + 4) % 8
            key[i] = ((v << shift) | (v >> shift)) & 0xFF
        self.key = bytes(key)

    def decrypt(self, data: bytes, offset: int) -> bytes:
        out = bytearray(data)
        for j in range(len(out)):
            p = offset + j
            if p > 0x7FFF:
                p %= 0x7FFF
            out[j] ^= self.key[p % 128]
        return bytes(out)


class QMC2RC4:
    """QMCv2 长密钥（>300 字节）分段 RC4 流密码。

    首段 0x80 字节逐字节独立取段密钥；其余按 0x1400 字节分段，段内用
    KSA 后状态预生成的 0x1400+512 keystream 切片异或，每段跳过
    get_segment_key(段号, seed, hash) & 0x1FF 字节。
    """

    SEG = 0x1400
    FIRST = 0x80

    def __init__(self, master: bytes):
        self.key = master
        n = len(master)
        # Go/Rust 里 state 是 []byte：state[i] = byte(i)，i ≥ 256 时按位回绕
        state = [i & 0xFF for i in range(n)]
        j = 0
        for i in range(n):
            j = (j + state[i] + master[i]) % n
            state[i], state[j] = state[j], state[i]
        h = 1
        for v in master:
            if v == 0:
                continue
            nxt = _u32(h * v)
            if nxt == 0 or nxt <= h:
                break
            h = nxt
        self.hash = h
        # 预生成 keystream：PRGA 从 KSA 后状态出发，产 0x1400+512 字节
        s = state[:]
        i = j = 0
        ks = bytearray()
        for _ in range(self.SEG + 512):
            i = (i + 1) % n
            j = (j + s[i]) % n
            s[i], s[j] = s[j], s[i]
            ks.append(s[(s[i] + s[j]) % n])
        self.ks = bytes(ks)

    def _seg_key(self, sid: int, seed: int) -> float:
        if seed == 0:
            return 0
        return math.trunc(self.hash / ((sid + 1) * seed) * 100.0)

    def _xor_segment(self, out: bytearray, start: int, take: int, sid: int, block_off: int):
        n = len(self.key)
        seed = self.key[sid % n]
        skip = int(self._seg_key(sid, seed)) & 0x1FF
        ks = self.ks[skip + block_off:skip + block_off + take]
        for j in range(take):
            out[start + j] ^= ks[j]

    def decrypt(self, data: bytes, offset: int) -> bytes:
        out = bytearray(data)
        n, N = len(self.key), len(out)
        pos, start = offset, 0
        if pos < self.FIRST:
            take = min(self.FIRST - pos, N - start)
            for j in range(take):
                o = pos + j
                idx = int(math.fmod(self._seg_key(o, self.key[o % n]), n))
                out[start + j] ^= self.key[idx]
            start += take
            pos += take
        if pos % self.SEG:
            rem = pos % self.SEG
            take = min(self.SEG - rem, N - start)
            self._xor_segment(out, start, take, pos // self.SEG, rem)
            start += take
            pos += take
        while start < N:
            take = min(self.SEG, N - start)
            self._xor_segment(out, start, take, pos // self.SEG, 0)
            start += take
            pos += take
        return bytes(out)


def new_cipher(master: bytes):
    return QMC2Map(master) if len(master) <= 300 else QMC2RC4(master)
