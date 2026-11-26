"""Generate a comment-heavy script: 24,000 comment lines, 1,860,930 bytes.
Usage: gen_comments.py OUTDIR [--decl]
With --decl a function, documented_fn, sits directly under the comment block
(1,860,953 bytes), so its doc comment is the whole block: the input for timing
docAbove."""
import os, sys
os.makedirs(sys.argv[1], exist_ok=True)
decl = "documented_fn() { :; }\n" if "--decl" in sys.argv[2:] else ""
open(os.path.join(sys.argv[1], "c.sh"), "w").write("#!/usr/bin/env bash\necho start\n" + "".join(
    f"# comment line number {i} with some text to pad it out to about forty bytes\n" for i in range(24000)) + decl + "echo end\n")
