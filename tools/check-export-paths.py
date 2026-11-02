"""Validate the exact-file manifest shared by the export build and release archive."""

import re
from pathlib import Path, PurePosixPath

# Uppercase Markdown filenames are reserved for public project metadata.
# Technical guides use descriptive filenames and still require manifest review.
PUBLIC_METADATA = {
    "CHANGELOG.md", "CODE_OF_CONDUCT.md", "CONTRIBUTING.md", "LICENSE.md",
    "NOTICE.md", "PULL_REQUEST_TEMPLATE.md", "README.md", "RELEASE_NOTES.md",
    "SECURITY.md", "THIRD_PARTY_NOTICES.md",
}

paths = set()
for line in Path("ci/public-export-files.txt").read_text().splitlines():
    if not line or line.startswith("#"):
        continue
    path = PurePosixPath(line)
    if (not re.fullmatch(r"[A-Za-z0-9._/-]+", line) or path.is_absolute()
            or line != path.as_posix() or ".." in line or line == "."):
        raise SystemExit(f"Unsafe export path: {line}")
    if any(part.lower() == "internal" for part in path.parts):
        raise SystemExit(f"Internal path in export manifest: {line}")
    if path.match("06_*_SETUP.txt") or path.match("*_TESTER.txt"):
        raise SystemExit(f"Local setup or review material in export manifest: {line}")
    for part in path.parts:
        name = PurePosixPath(part)
        if name.suffix.lower() == ".md" and name.stem.isupper() and part not in PUBLIC_METADATA:
            raise SystemExit(f"Unrecognised project metadata in export manifest: {line}")
    if line in paths:
        raise SystemExit(f"Duplicate export path: {line}")
    paths.add(line)

if not paths:
    raise SystemExit("Empty export manifest")
