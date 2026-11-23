"""Generate a comment-heavy script: 24,000 comment lines, 1,860,930 bytes. Usage: gen_comments.py OUTDIR"""
import os, sys
os.makedirs(sys.argv[1], exist_ok=True)
open(os.path.join(sys.argv[1], "c.sh"), "w").write("#!/usr/bin/env bash\necho start\n" + "".join(
    f"# comment line number {i} with some text to pad it out to about forty bytes\n" for i in range(24000)) + "echo end\n")
