#!/usr/bin/env python3
from pathlib import Path
import sys

if len(sys.argv) != 2:
    print('usage: patch-nginx-map.py <nginx.conf>', file=sys.stderr)
    sys.exit(2)

p = Path(sys.argv[1])
text = p.read_text(encoding='utf-8')
if 'connection_upgrade' in text:
    sys.exit(0)

lines = text.splitlines()
out = []
inserted = False
for line in lines:
    out.append(line)
    if (not inserted) and line.strip() == 'http {':
        out.append('    map $http_upgrade $connection_upgrade {')
        out.append('        default upgrade;')
        out.append("        ''      close;")
        out.append('    }')
        inserted = True

if not inserted:
    print('http { block not found', file=sys.stderr)
    sys.exit(1)

p.write_text('\n'.join(out) + '\n', encoding='utf-8')
