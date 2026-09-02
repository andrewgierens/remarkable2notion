#!/usr/bin/env python3
"""Generate send-to-notion-debug.qmd from send-to-notion.qmd.

The debug build is the same patch with console.warn("[rmn] ...") traces at
every link in the chain: row tapped -> signal emitted -> handler reached ->
rmNotionOpenOverlay -> overlay opened -> each bridge call -> targets loaded.
Read them on device with:

    journalctl -u xochitl -b --no-pager | grep -a '\\[rmn\\]'

Generating it means the two files cannot drift apart, which is what made an
earlier debug build trace code that the real patch no longer had.
"""
import pathlib
import sys

HERE = pathlib.Path(__file__).resolve().parent.parent
SRC = HERE / "send-to-notion.qmd"
DST = HERE / "send-to-notion-debug.qmd"

# (anchor, trace) — the trace is inserted immediately after the anchor, which
# must appear exactly once in the source.
TRACES = [
    (
        '                var out = ("" + (env.stdout || "")).trim()\n',
        '                console.warn("[rmn] call " + method + " out=<" + out + "> err=<" + env.stderr + ">");\n',
    ),
    (
        '                rmnAccounts = r.result.accounts || []\n',
        '                console.warn("[rmn] targets accounts=" + rmnAccounts.length\n'
        '                             + " listCount=" + rmnList.count\n'
        '                             + " listH=" + rmnList.height);\n',
    ),
    (
        '                rmnPageRange.text = range || ""\n',
        '                console.warn("[rmn] overlay.rmnOpen uuid=" + uuid + " parent=" + parent\n'
        '                             + " w=" + width + " h=" + height);\n',
    ),
    (
        '                visible = true\n',
        '                console.warn("[rmn] overlay visible=" + visible + " z=" + z);\n',
    ),
    (
        '            function rmNotionOpenOverlay() {\n',
        '                console.warn("[rmn] rmNotionOpenOverlay; overlay=" + rmNotionOverlay);\n',
    ),
    (
        '                onSendToNotionSelected: {\n',
        '                    console.warn("[rmn] handler reached on Toolbar");\n',
    ),
]

# (old, new) — whole-line rewrites, for traces that do not fit "insert after
# an anchor". Each old must appear exactly once.
REPLACEMENTS = [
    (
        "                        onClicked: root.toolbar.sendToNotionSelected()\n",
        "                        onClicked: {\n"
        '                            console.warn("[rmn] row tapped; toolbar=" + root.toolbar);\n'
        "                            root.toolbar.sendToNotionSelected();\n"
        '                            console.warn("[rmn] signal emitted");\n'
        "                        }\n",
    ),
]

HEADER = (
    "; GENERATED FILE — do not edit. Regenerate after changing\n"
    "; send-to-notion.qmd:\n"
    ";\n"
    ";     python3 app/qmd/scripts/make-debug.py\n"
    ";\n"
)


def main() -> int:
    text = SRC.read_text()
    for old, new in REPLACEMENTS:
        if text.count(old) != 1:
            print(
                f"make-debug: replacement appears {text.count(old)} times, want 1:\n"
                f"{old!r}",
                file=sys.stderr,
            )
            return 1
        text = text.replace(old, new, 1)
    for anchor, trace in TRACES:
        if text.count(anchor) != 1:
            print(
                f"make-debug: anchor appears {text.count(anchor)} times, want 1:\n"
                f"{anchor!r}",
                file=sys.stderr,
            )
            return 1
        text = text.replace(anchor, anchor + trace, 1)
    DST.write_text(HEADER + text)
    print(f"make-debug: wrote {DST.relative_to(HERE.parent.parent)}")
    return 0


if __name__ == "__main__":
    sys.exit(main())
