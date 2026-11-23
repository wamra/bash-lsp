# Reproductions for the open deep-nesting crash (review C1)

Written by the fix-wave re-reviewer on 2026-11-22 against a binary built from
c384698. `gen.py`, `gen2.py`, `gen3.py` write the crashing inputs and `fp.py`
the wrongly refused ones; they write under `/private/tmp/wbrr/` (edit `base`
to change that). `run.sh CASE` runs `./bash-lsp symbols cases/CASE --json`
with a 120 s alarm. Run the generators with `uv run --no-project python`.
See `docs/m1-deferred.md`, last section.
