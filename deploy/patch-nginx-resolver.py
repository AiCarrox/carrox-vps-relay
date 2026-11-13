#!/usr/bin/env python3
"""Insert a `resolver` directive at the top of the http {} block.

Managed sites use a variable in proxy_pass so that an unresolvable upstream
cannot fail `nginx -t` for the whole host. That defers DNS to request time,
which nginx only does when a resolver is configured.
"""
from pathlib import Path
import re
import sys

if len(sys.argv) != 2:
    print('usage: patch-nginx-resolver.py <nginx.conf>', file=sys.stderr)
    sys.exit(2)

p = Path(sys.argv[1])
text = p.read_text(encoding='utf-8')
if re.search(r'^\s*resolver\s', text, re.M):
    sys.exit(0)

lines = text.splitlines()
out = []
inserted = False
for line in lines:
    out.append(line)
    if (not inserted) and line.strip() == 'http {':
        out.append('    resolver 1.1.1.1 8.8.8.8 valid=300s ipv6=off;')
        out.append('    resolver_timeout 5s;')
        inserted = True

if not inserted:
    print('http { block not found', file=sys.stderr)
    sys.exit(1)

p.write_text('\n'.join(out) + '\n', encoding='utf-8')
