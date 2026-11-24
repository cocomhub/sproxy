#!/usr/bin/env python3
# SPDX-License-Identifier: Apache-2.0
"""按状态批量截图（设计评审用，输出到系统临时目录，不入库）。"""
import os
import subprocess
import json
from pathlib import Path

BASE = Path("docs/designs/mockups/webui-v2-prototype.html")
SHOT_DIR = Path(os.environ["TEMP"]) / "webui-shots"
CHROME = r"C:\Program Files\Google\Chrome\Application\chrome.exe"
SHOT_DIR.mkdir(parents=True, exist_ok=True)
html_base = BASE.read_text(encoding="utf-8")

specs = []
for skin in "abc":
    specs += [
        (f"{skin}-files", skin, "light", "files", "", 0),
        (f"{skin}-grid", skin, "light", "grid", "", 0),
        (f"{skin}-transfer", skin, "light", "transfer", "", 0),
        (f"{skin}-trash", skin, "light", "trash", "", 0),
        (f"{skin}-console-overview", skin, "light", "console", "overview", 0),
        (f"{skin}-console-cloud", skin, "light", "console", "cloud", 0),
    ]
    for st, clicks in (("empty", 1), ("error", 2), ("loading", 3)):
        specs.append((f"{skin}-console-{st}", skin, "light", "console", "cloud", clicks))
specs += [
    ("a-dark-files", "a", "dark", "files", "", 0),
    ("c-dark-console", "c", "dark", "console", "overview", 0),
    ("m-a-files", "a", "light", "files", "", 0),
    ("m-c-console", "c", "light", "console", "overview", 0),
]

results = []
for name, skin, theme, view, sec, clicks in specs:
    js = f"document.documentElement.dataset.skin='{skin}';document.documentElement.dataset.theme='{theme}';"
    if view == "grid":
        js += "document.querySelector('[data-view=\"grid\"]').click();"
    elif view in ("files", "transfer", "trash"):
        js += f"document.querySelector('.nav[data-goto=\"{view}\"]').click();"
    elif view == "console":
        js += "document.querySelector('.nav[data-goto=\"console\"]').click();"
        if sec:
            js += f"document.querySelector('.cnav[data-sec=\"{sec}\"]').click();"
        if clicks:
            js += f"for(var i=0;i<{clicks};i++){{document.getElementById('dev-states').click();}}"
    inject = f"<script>setTimeout(function(){{{js}}},200);</script>"
    src = SHOT_DIR / (name + ".html")
    src.write_text(html_base.replace("</body>", inject + "\n</body>"), encoding="utf-8")
    png = SHOT_DIR / (name + ".png")
    size = "390,844" if name.startswith("m-") else "1500,950"
    subprocess.run(
        [CHROME, "--headless=new", "--disable-gpu", "--hide-scrollbars",
         "--window-size=" + size, "--virtual-time-budget=2500",
         "--screenshot=" + str(png), Path(src).as_uri()],
        stdout=subprocess.DEVNULL, stderr=subprocess.DEVNULL, timeout=60)
    results.append((name, png.stat().st_size if png.exists() else 0))

print(json.dumps({"count": len(results), "min_size": min(r[1] for r in results),
                  "max_size": max(r[1] for r in results), "dir": str(SHOT_DIR),
                  "blank": [r[0] for r in results if r[1] < 5000]}, ensure_ascii=False))
