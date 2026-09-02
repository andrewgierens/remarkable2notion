#!/usr/bin/env bash
# Sanity checks for the QMLDiff patch. Structural checks always run; if a
# qmldiff binary is available (QMLDIFF=... or on PATH — build it from
# github.com/asivery/qmldiff), the diff is additionally applied against the
# mock QML root in ../testdata to prove it parses and applies.
set -euo pipefail

qmd_dir="$(cd "$(dirname "$0")/.." && pwd)"
status=0

shopt -s nullglob
qmds=("$qmd_dir"/*.qmd)
if [[ ${#qmds[@]} -eq 0 ]]; then
    echo "validate: no .qmd files found in $qmd_dir" >&2
    exit 1
fi

count_of() { grep -cE "$2" "$1" || true; }

for f in "${qmds[@]}"; do
    name="$(basename "$f")"
    if [[ ! -s "$f" ]]; then
        echo "validate: $name is empty" >&2
        status=1
    fi
    if grep -qU $'\r' "$f"; then
        echo "validate: $name has CRLF line endings" >&2
        status=1
    fi
    if grep -qE '^(<<<<<<<|=======$|>>>>>>>)' "$f"; then
        echo "validate: $name contains merge conflict markers" >&2
        status=1
    fi
    # Paired block directives.
    for pair in "SLOT:END SLOT" "AFFECT:END AFFECT" "TRAVERSE:END TRAVERSE"; do
        open="${pair%%:*}"; close="${pair#*:}"
        opens=$(count_of "$f" "^[[:space:]]*$open ")
        closes=$(count_of "$f" "^[[:space:]]*$close\$")
        if [[ "$opens" != "$closes" ]]; then
            echo "validate: $name: $opens '$open' vs $closes '$close'" >&2
            status=1
        fi
    done
    # Balanced braces across the whole file.
    opens=$(grep -o '{' "$f" | wc -l)
    closes=$(grep -o '}' "$f" | wc -l)
    if [[ "$opens" != "$closes" ]]; then
        echo "validate: $name: unbalanced braces ($opens vs $closes)" >&2
        status=1
    fi
done

qmldiff_bin="${QMLDIFF:-$(command -v qmldiff || true)}"
if [[ -n "$qmldiff_bin" ]]; then
    out="$(mktemp -d)"
    trap 'rm -rf "$out"' EXIT
    if "$qmldiff_bin" apply-diffs -c "$qmd_dir/testdata" "$out" "${qmds[@]}" >/dev/null 2>&1; then
        echo "validate: qmldiff applied ${#qmds[@]} diff(s) against testdata OK"
    else
        echo "validate: qmldiff failed to apply the diffs:" >&2
        "$qmldiff_bin" apply-diffs -c "$qmd_dir/testdata" "$out" "${qmds[@]}" >&2 || true
        status=1
    fi
else
    echo "validate: qmldiff binary not found — structural checks only"
fi

if [[ $status -eq 0 ]]; then
    echo "validate: ${#qmds[@]} qmd file(s) OK"
fi
exit $status
