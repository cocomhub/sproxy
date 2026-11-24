#!/usr/bin/env python3
# SPDX-License-Identifier: Apache-2.0
"""WCAG 对比度审计：解析原型中的 6 套主题 token，检查正文/状态色/交互边界。

用法：python docs/designs/mockups/audit_contrast.py
退出码：0 全部通过；1 存在不达标项（便于挂进本地检查）。
"""
import re
import sys

SRC = "docs/designs/mockups/webui-v2-prototype.html"
PAIRS = [
    ("text", "surface", 4.5), ("text-2", "surface", 4.5), ("text-3", "surface", 4.5),
    ("text", "bg", 4.5), ("text-2", "bg", 4.5), ("text-3", "bg", 4.5),
    ("accent-fg", "accent", 4.5), ("accent", "surface", 3.0),
    ("ok", "surface", 4.5), ("warn", "surface", 4.5), ("bad", "surface", 4.5), ("info", "surface", 4.5),
    ("border-strong", "surface", 3.0),
]


def parse(value):
    value = value.strip()
    m = re.match(r"#([0-9a-fA-F]{6})$", value)
    if m:
        return tuple(int(m.group(1)[i:i + 2], 16) for i in (0, 2, 4)), 1.0
    m = re.match(r"rgba?\(([^)]+)\)", value)
    if m:
        parts = [p.strip() for p in m.group(1).split(",")]
        return tuple(int(float(p)) for p in parts[:3]), (float(parts[3]) if len(parts) > 3 else 1.0)
    return None, None


def over(fg, alpha, bg):
    return tuple(round(fg[i] * alpha + bg[i] * (1 - alpha)) for i in range(3))


def luminance(c):
    def f(x):
        x /= 255
        return x / 12.92 if x <= 0.03928 else ((x + 0.055) / 1.055) ** 2.4
    r, g, b = (f(x) for x in c)
    return 0.2126 * r + 0.7152 * g + 0.0722 * b


def ratio(a, b):
    la, lb = luminance(a), luminance(b)
    hi, lo = max(la, lb), min(la, lb)
    return (hi + 0.05) / (lo + 0.05)


def main():
    src = open(SRC, encoding="utf-8").read()
    blocks = re.findall(r'\[data-skin="([abc])"\]\[data-theme="(dark|light)"\]\s*\{(.*?)\n\}', src, re.S)
    failures = 0
    for skin, theme, body in blocks:
        props = dict(re.findall(r"(--[a-z0-9-]+)\s*:\s*([^;]+);", body))
        bg, _ = parse(props["--bg"])
        surf, a = parse(props["--surface"])
        surf = over(surf, a, bg)
        for fg_key, bg_key, need in PAIRS:
            if "--" + fg_key not in props or "--" + bg_key not in props:
                continue
            fg, fa = parse(props["--" + fg_key])
            bb, ba = parse(props["--" + bg_key])
            if bg_key == "surface":
                bb = surf
            else:
                bb = over(bb, ba, bg)
            fg = over(fg, fa, bb)
            r = ratio(fg, bb)
            ok = r >= need
            if not ok:
                failures += 1
            print(f"{'PASS' if ok else 'FAIL'} {skin}/{theme} {fg_key}/{bg_key} = {r:.2f} (need {need})")
    print(f"\n{'OK' if not failures else 'FAILED'}: {failures} failing pair(s)")
    return 1 if failures else 0


if __name__ == "__main__":
    sys.exit(main())
