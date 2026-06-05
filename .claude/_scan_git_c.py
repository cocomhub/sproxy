# Copyright 2026 The Cocomhub Authors. All rights reserved.
# SPDX-License-Identifier: Apache-2.0

import json, os, re
from collections import Counter

git_c_patterns = Counter()
go_test_patterns = Counter()

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
                    m = re.match(r"\s*git\s+-C\s+\S+\s+(\S+)", cmd)
                    if m:
                        git_c_patterns["git -C <path> " + m.group(1)] += 1
                    m = re.match(r"\s*go\s+test\s+(.+)", cmd)
                    if m:
                        go_test_patterns[m.group(1)[:80]] += 1

print("=== git -C <path> subcommands ===")
for k, v in git_c_patterns.most_common():
    print(f"{v:4d} {k}")
print()
print("=== go test patterns ===")
for k, v in go_test_patterns.most_common():
    print(f"{v:4d} go test {k}")