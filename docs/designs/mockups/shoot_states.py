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
    specs += [
        spec(f"{skin}-queue", skin, actions=[
            "document.querySelector('[data-goto=transfer]').click()",
            "document.getElementById('open-queue').click()"]),
        spec(f"{skin}-groups", skin, actions=[
            "document.querySelector('[data-goto=transfer]').click()"]),
        spec(f"{skin}-group-detail", skin, actions=[
            "document.querySelector('[data-goto=transfer]').click()",
            "document.querySelector('[data-dialog=dlg-group]').click()"]),
        spec(f"{skin}-onboarding", skin, actions=[
            "document.getElementById('dev-onboard').click()"]),
        spec(f"{skin}-readonly", skin, actions=[
            "document.getElementById('dev-ro').click()"]),
        spec(f"{skin}-share", skin, actions=[
            "document.getElementById('dev-share').click()"]),
    ]
    specs += [
        spec(f"{skin}-notify", skin, actions=[
            "document.querySelector('[data-goto=console]').click()",
            "document.querySelector('[data-sec=notify]').click()"]),
        spec(f"{skin}-files-en", skin, actions=[
            "document.getElementById('lang-btn').click()",
            "document.querySelector('[data-lang-opt=\"en-US\"]').click()"]),
        spec(f"{skin}-volume-menu", skin, actions=[
            "document.getElementById('volume-btn').click()"]),
        spec(f"{skin}-cloud-batch", skin, actions=[
            "document.querySelector('[data-goto=console]').click()",
            "document.querySelector('[data-sec=cloud]').click()",
            "var c=document.querySelector('#cloud-table tbody .cb');c.checked=true;c.dispatchEvent(new Event('change',{bubbles:true}));"]),
        spec(f"{skin}-dialog-sync", skin, actions=[
            "document.querySelector('[data-goto=console]').click()",
            "document.querySelector('[data-sec=sync]').click()",
            "document.querySelector('[data-dialog=dlg-sync]').click()"]),
        spec(f"{skin}-dialog-group-create", skin, actions=[
            "document.querySelector('[data-goto=transfer]').click()",
            "document.querySelector('[data-dialog=dlg-group-create]').click()"]),
        spec(f"{skin}-dialog-archive", skin, actions=[
            "document.querySelector('[data-dialog=dlg-archive]').click()"]),
        spec(f"{skin}-console-access", skin, actions=[
            "document.querySelector('[data-goto=console]').click()",
            "document.querySelector('[data-sec=access]').click()"]),
        spec(f"{skin}-wizard-rotate", skin, actions=[
            "document.querySelector('[data-goto=console]').click()",
            "document.querySelector('[data-sec=credentials]').click()",
            "document.querySelector('[data-dialog=dlg-rotate]').click()"]),
        spec(f"{skin}-dialog-audit", skin, actions=[
            "document.querySelector('[data-goto=console]').click()",
            "document.querySelector('[data-dialog=dlg-audit]').click()"]),
        spec(f"{skin}-dialog-config", skin, actions=[
            "document.querySelector('[data-goto=console]').click()",
            "document.querySelector('[data-sec=config]').click()",
            "document.querySelector('[data-dialog=dlg-config]').click()"]),
        spec(f"{skin}-task-source", skin, actions=[
            "document.querySelector('[data-goto=transfer]').click()",
            "document.querySelector('[data-task]').click()"]),
        spec(f"{skin}-preview-video", skin, actions=[
            "document.querySelector('[data-preview]').click()"]),
        spec(f"{skin}-preview-text", skin, actions=[
            "document.getElementById('dev-preview').click()",
            "document.getElementById('dev-preview').click()",
            "document.getElementById('dev-preview').click()"]),
        spec(f"{skin}-error-404", skin, actions=[
            "document.getElementById('dev-error').click()"]),
        spec(f"{skin}-wizard-volume", skin, actions=[
            "document.querySelector('[data-dialog=dlg-volume]').click()"]),
        spec(f"{skin}-console-verify", skin, actions=[
            "document.querySelector('[data-goto=console]').click()",
            "document.querySelector('[data-sec=verify]').click()"]),
        spec(f"{skin}-share-pw-error", skin, actions=[
            "document.getElementById('dev-share').click()",
            "document.getElementById('share-pw').value='';",
            "document.getElementById('share-dl').click()"]),
        spec(f"{skin}-dialog-rule", skin, actions=[
            "document.querySelector('[data-goto=console]').click()",
            "document.querySelector('[data-sec=notify]').click()",
            "document.querySelector('[data-dialog=dlg-rule]').click()"]),
        spec(f"{skin}-search", skin, actions=[
            "document.getElementById('search-trigger').click()",
            "var i=document.getElementById('cmdk-input');i.value='NHDTB';i.dispatchEvent(new Event('input',{bubbles:true}));"]),
    ]
    specs.append(spec(f"{skin}-session-expired", skin, actions=[
        "document.body.dataset.session='expired';",
        'document.querySelector(\'.nav[data-goto="files"]\').click()']))

specs += [
    spec("a-dark-files", "a", "dark", ['document.querySelector(\'.nav[data-goto="files"]\').click()']),
    spec("c-dark-console", "c", "dark", ['document.querySelector(\'.nav[data-goto="console"]\').click()']),
    spec("m-a-files", "a", actions=['document.querySelector(\'.nav[data-goto="files"]\').click()']),
    spec("m-c-console", "c", actions=['document.querySelector(\'.nav[data-goto="console"]\').click()']),
    spec("m-a-auth", "a", actions=["document.getElementById('dev-auth').click()"]),
    spec("m-c-sheet", "c", actions=["document.querySelector('[data-sheet]').click()"]),
    spec("m-a-search", "a", actions=[
        "document.getElementById('search-trigger').click()",
        "var i=document.getElementById('cmdk-input');i.value='MGT';i.dispatchEvent(new Event('input',{bubbles:true}));"]),
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
