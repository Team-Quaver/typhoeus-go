#!/usr/bin/env python3
# -*- coding: utf-8 -*-
"""用 unlock-music 对拍的纯 Python 参考实现，为 Go qmc 包生成跨语言对拍向量。

用法：python3 tools/gen_qmc_vectors.py > qmc/vectors.json
Go 侧：QMC_CROSS_VECTORS=qmc/vectors.json go test ./qmc/ -run CrossVectors
"""
import base64
import json
import random
import sys
from pathlib import Path

sys.path.insert(0, str(Path(__file__).resolve().parent))
import qmc_ref as mzj  # noqa: E402  参考实现（unlock-music 对拍移植，tools/qmc_ref.py）


def rand_bytes(rng, n):
    return bytes(rng.randrange(256) for _ in range(n))


def build_v2_ekey(master, rng):
    """构造 API 同款 EKey：前缀 UVFNdXNpYyBFbmNWMixLZXk6 = base64("QQMusic EncV2,Key:")。

    V1 内层：base64( master[:8] + TEA(interleave(simple_key, master[:8]), master[8:]) )
    V2 外层：内层 base64 文本 + 若干 0，先 TEA(KEY2) 再 TEA(KEY1) 后整体 base64。
    """
    header, body = master[:8], master[8:]
    tea_key = bytearray()
    for sk, hk in zip(mzj.EKEY_SIMPLE_KEY, header):
        tea_key.append(sk)
        tea_key.append(hk)
    cipher = mzj.tea_cbc_encrypt(body, bytes(tea_key), rand_bytes(rng, 10))
    v1_text = base64.b64encode(header + cipher)

    payload = bytearray(v1_text) + b"\x00" * rng.randrange(1, 8)
    layer2 = mzj.tea_cbc_encrypt(bytes(payload), bytes(mzj.EKEY_V2_KEY2), rand_bytes(rng, 10))
    layer1 = mzj.tea_cbc_encrypt(layer2, bytes(mzj.EKEY_V2_KEY1), rand_bytes(rng, 10))
    return mzj.EKEY_V2_PREFIX.decode() + base64.b64encode(layer1).decode()


def main():
    rng = random.Random(20260928)
    vectors = []
    for _ in range(8):
        master_len = rng.choice([16, 64, 128, 296, 301, 512])
        master = rand_bytes(rng, master_len)
        offset = rng.choice([0, 1, 0x7F, 0x80, 0x1400, 0x2801, 32760, 32768, 999999])
        take = 128
        cipher = rand_bytes(rng, take)
        if master_len <= 300:
            c = mzj.QMC2Map(master)
        else:
            c = mzj.QMC2RC4(master)
        plain = c.decrypt(cipher, offset)
        vectors.append({
            "ekey": build_v2_ekey(master, rng),
            "master": master.hex(),
            "cipher": cipher.hex(),
            "plain": plain.hex(),
            "offset": offset,
        })
    json.dump(vectors, sys.stdout, indent=1)


if __name__ == "__main__":
    main()
