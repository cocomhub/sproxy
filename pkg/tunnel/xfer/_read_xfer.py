#!/usr/bin/env python
with open('xfer.go', 'r', encoding='utf-8') as f:
    lines = f.readlines()
for i, line in enumerate(lines, 1):
    print(f"{i:4d}: {line}", end='')
