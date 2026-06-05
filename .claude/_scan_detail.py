# Copyright 2026 The Cocomhub Authors. All rights reserved.
# SPDX-License-Identifier: Apache-2.0

import json, os, re
from collections import Counter

bash_detail = Counter()
files = []
for root, dirs, fnames in os.walk("C:/Users/leon.li/.claude/projects"):
    for f in fnames:
        if f.endswith(".jsonl"):
            fp = os.path.join(root, f)
            files.append((os.path.getmtime(fp), fp))
files.sort(reverse=True)
recent = [fp for _, fp in files[:50]]

for fp in recent:
    with open(fp, "r", encoding="utf-8", errors="replace") as fh:
        for line in fh:
            line = line.strip()
            if not line: continue
            try: obj = json.loads(line)
            except: continue
            if (obj.get("type") or obj.get("role")) not in ("assistant",): continue
            content = obj.get("message", obj).get("content", [])
            if isinstance(content, str): continue
            for block in content:
                if not isinstance(block, dict): continue
                if block.get("type") == "tool_use" and block.get("name") == "Bash":
                    cmd = block.get("input", {}).get("command", "").strip()
                    # Git subcommands
                    m = re.match(r"\s*git\s+(\S+)", cmd)
                    if m:
                        bash_detail["git " + m.group(1)] += 1
                    # Go subcommands
                    m = re.match(r"\s*go\s+(\S+)", cmd)
                    if m:
                        bash_detail["go " + m.group(1)] += 1

print("=== Git subcommands ===")
for k, v in bash_detail.most_common():
    if k.startswith("git"):
        print(f"{v:4d} {k}")
print()
print("=== Go subcommands ===")
for k, v in bash_detail.most_common():
    if k.startswith("go"):
        print(f"{v:4d} {k}")