# Copyright 2026 The Cocomhub Authors. All rights reserved.
# SPDX-License-Identifier: Apache-2.0

import sys
with open('pkg/server/cloud_archive_handler_test.go', 'r', encoding='utf-8') as f:
    content = f.read()

old = '.tar.gz", got %q", result.File)\n\t}\n\tif !strings.HasSuffix(result.File, ".tar.gz") {\n\t\tt.Fatalf("expected archive name ending with'
new = '.tar.gz", got %q", result.File)\n\t}\n\tif result.Size <= 0 {'

if old in content:
    content = content.replace(old, new, 1)
    with open('pkg/server/cloud_archive_handler_test.go', 'w', encoding='utf-8') as f:
        f.write(content)
    print("OK")
else:
    print("NOTFOUND")
    idx = content.find('expected archive name ending with')
    if idx >= 0:
        print(repr(content[idx:idx+350]))
    sys.exit(1)