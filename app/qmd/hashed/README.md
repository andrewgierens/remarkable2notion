# Hashed qmd builds

Per-OS-version hashed builds of `../send-to-notion.qmd` — the distribution
format qt-resource-rebuilder consumes on device (identifiers obfuscated per
the qmldiff hashing scheme; the device's hashtab translates them back).
Regenerate after editing the source diff:

    qmldiff hash-diffs <hashtab-for-that-OS> send-to-notion.qmd

The plain file in `app/qmd/` stays the source of truth.
