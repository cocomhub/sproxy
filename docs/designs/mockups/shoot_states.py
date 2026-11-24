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


def spec(name, skin, theme="light", actions=()):
    return {"name": name, "skin": skin, "theme": theme, "actions": list(actions)}


specs = []
for skin in "abc":
    specs += [
        spec(f"{skin}-files", skin, actions=['document.querySelector(\'.nav[data-goto="files"]\').click()']),
        spec(f"{skin}-grid", skin, actions=['document.querySelector(\'[data-view="grid"]\').click()']),
        spec(f"{skin}-transfer", skin, actions=['document.querySelector(\'.nav[data-goto="transfer"]\').click()']),
        spec(f"{skin}-trash", skin, actions=['document.querySelector(\'.nav[data-goto="trash"]\').click()']),
        spec(f"{skin}-console-overview", skin, actions=['document.querySelector(\'.nav[data-goto="console"]\').click()']),
        spec(f"{skin}-console-cloud", skin, actions=[
            'document.querySelector(\'.nav[data-goto="console"]\').click()',
            'document.querySelector(\'.cnav[data-sec="cloud"]\').click()']),
        spec(f"{skin}-auth-login", skin, actions=["document.getElementById('dev-auth').click()"]),
        spec(f"{skin}-auth-register", skin, actions=[
            "document.getElementById('dev-auth').click()",
            'document.querySelector(\'[data-auth-tab="register"]\').click()']),
        spec(f"{skin}-account-menu", skin, actions=["document.getElementById('account-btn').click()"]),
        spec(f"{skin}-dialog-mkdir", skin, actions=[
            'document.querySelector(\'[data-dialog="dlg-mkdir"]\').click()']),
        spec(f"{skin}-dialog-rename", skin, actions=[
            'document.querySelector(\'[data-dialog="dlg-rename"]\').click()']),
        spec(f"{skin}-dialog-batch", skin, actions=[
            'document.querySelector(\'[data-dialog="dlg-batch"]\').click()']),
    ]
    for st, clicks in (("empty", 1), ("error", 2), ("loading", 3)):
        specs.append(spec(f"{skin}-console-{st}", skin, actions=[
            'document.querySelector(\'.nav[data-goto="console"]\').click()',
            'document.querySelector(\'.cnav[data-sec="cloud"]\').click()',
            f"for(var i=0;i<{clicks};i++){{document.getElementById('dev-states').click();}}"]))
    specs.append(spec(f"{skin}-session-expired", skin, actions=[
        "document.body.dataset.session='expired';",
        'document.querySelector(\'.nav[data-goto="files"]\').click()']))

specs += [
    spec("a-dark-files", "a", "dark", ['document.querySelector(\'.nav[data-goto="files"]\').click()']),
    spec("c-dark-console", "c", "dark", ['document.querySelector(\'.nav[data-goto="console"]\').click()']),
    spec("m-a-files", "a", actions=['document.querySelector(\'.nav[data-goto="files"]\').click()']),
    spec("m-c-console", "c", actions=['document.querySelector(\'.nav[data-goto="console"]\').click()']),
    spec("m-a-auth", "a", actions=["document.getElementById('dev-auth').click()"]),
]

results = []
for sp in specs:
    js = f"document.documentElement.dataset.skin='{sp['skin']}';document.documentElement.dataset.theme='{sp['theme']}';"
    js += "".join(sp["actions"])
    inject = f"<script>setTimeout(function(){{{js}}},200);</script>"
    src = SHOT_DIR / (sp["name"] + ".html")
    src.write_text(html_base.replace("</body>", inject + "\n</body>"), encoding="utf-8")
    png = SHOT_DIR / (sp["name"] + ".png")
    size = "390,844" if sp["name"].startswith("m-") else "1500,950"
    subprocess.run(
        [CHROME, "--headless=new", "--disable-gpu", "--hide-scrollbars",
         "--window-size=" + size, "--virtual-time-budget=2800",
         "--screenshot=" + str(png), Path(src).as_uri()],
        stdout=subprocess.DEVNULL, stderr=subprocess.DEVNULL, timeout=60)
    results.append((sp["name"], png.stat().st_size if png.exists() else 0))

print(json.dumps({"count": len(results), "min_size": min(r[1] for r in results),
                  "max_size": max(r[1] for r in results), "dir": str(SHOT_DIR),
                  "blank": [r[0] for r in results if r[1] < 5000]}, ensure_ascii=False))
