# Copyright 2026 The Cocomhub Authors. All rights reserved.
# SPDX-License-Identifier: Apache-2.0

import json, os, glob, re
from collections import Counter
from datetime import datetime

bash_counts = Counter()
mcp_counts = Counter()

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
            if not line:
                continue
            try:
                obj = json.loads(line)
            except:
                continue
            msg_type = obj.get("type") or obj.get("role")
            if msg_type not in ("assistant",):
                continue
            content = obj.get("message", obj).get("content", [])
            if isinstance(content, str):
                continue
            for block in content:
                if not isinstance(block, dict):
                    continue
                if block.get("type") == "tool_use":
                    name = block.get("name", "")
                    inp = block.get("input", {})
                    if name == "Bash":
                        cmd = inp.get("command", "").strip()
                        if cmd:
                            m = re.match(r'(?:\w+=\S+\s+)*(?:sudo\s+|timeout\s+\d+\s+)*(\S+)', cmd)
                            if m:
                                first = m.group(1)
                                bash_counts[first] += 1
                            else:
                                bash_counts[cmd.split()[0]] += 1
                    elif name.startswith("mcp__"):
                        mcp_counts[name] += 1

print("=== Bash Tool Calls ===")
for cmd, cnt in bash_counts.most_common(50):
    print(f"{cnt:4d} {cmd}")

print()
print("=== MCP Tool Calls ===")
for name, cnt in mcp_counts.most_common(50):
    print(f"{cnt:4d} {name}")