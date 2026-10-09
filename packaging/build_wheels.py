#!/usr/bin/env python3
"""Turn the pure polign-recall wheel into one platform wheel per release archive.

`python -m build` makes polign_recall-X.Y.Z-py3-none-any.whl: the Python
client alone. This script copies that wheel once per platform, adds the
`recall` binary from the matching signed release archive to the wheel's
scripts, and retags it, so `pip install polign-recall` brings the client and
the server it runs, the way the ruff and uv wheels ship their binaries.
Nothing is compiled. The pure wheel stays published for every other platform,
where the client runs `recall` from PATH.

    python -m build python --outdir dist
    python packaging/build_wheels.py --wheel dist/polign_recall-0.12.0-py3-none-any.whl \
        --archives ./release --out dist

Each archive is checked against the release's checksums.txt first.
Standard library only.
"""

from __future__ import annotations

import argparse
import base64
import hashlib
import io
import re
import tarfile
import zipfile
from pathlib import Path

BINARY = "recall"

# Release archive -> wheel platform tags. The binary is static (CGO off), so
# one Linux build serves glibc and musl. Go 1.25 needs macOS 12.
PLATFORMS = {
    "recall_darwin_arm64.tar.gz": ["macosx_12_0_arm64"],
    "recall_darwin_amd64.tar.gz": ["macosx_12_0_x86_64"],
    "recall_linux_amd64.tar.gz": ["manylinux2014_x86_64", "manylinux_2_17_x86_64", "musllinux_1_1_x86_64"],
    "recall_linux_arm64.tar.gz": ["manylinux2014_aarch64", "manylinux_2_17_aarch64", "musllinux_1_1_aarch64"],
    "recall_windows_amd64.zip": ["win_amd64"],
    "recall_windows_arm64.zip": ["win_arm64"],
}


def checksums(text: str) -> dict[str, str]:
    out = {}
    for line in text.splitlines():
        parts = line.split()
        if len(parts) == 2:
            out[parts[1].lstrip("*")] = parts[0].lower()
    return out


def binary(archive_name: str, data: bytes) -> bytes:
    """The recall executable inside a release archive."""
    want = BINARY + (".exe" if archive_name.endswith(".zip") else "")
    if archive_name.endswith(".zip"):
        with zipfile.ZipFile(io.BytesIO(data)) as zf:
            for info in zf.infolist():
                if info.filename.rsplit("/", 1)[-1] == want and not info.is_dir():
                    return zf.read(info)
    else:
        with tarfile.open(fileobj=io.BytesIO(data), mode="r:gz") as tf:
            for member in tf:
                if member.name.rsplit("/", 1)[-1] == want and member.isfile():
                    return tf.extractfile(member).read()  # type: ignore[union-attr]
    raise SystemExit(f"{archive_name} has no {want}")


def record_line(path: str, blob: bytes) -> str:
    digest = base64.urlsafe_b64encode(hashlib.sha256(blob).digest()).rstrip(b"=").decode()
    return f"{path},sha256={digest},{len(blob)}\n"


def platform_wheel(pure: Path, archive_name: str, exe: bytes, out: Path) -> Path:
    """A copy of the pure wheel with recall added and the platform tags set."""
    match = re.fullmatch(r"(polign_recall)-([^-]+)-py3-none-any\.whl", pure.name)
    if not match:
        raise SystemExit(f"{pure.name} is not a pure polign_recall wheel")
    name, version = match.groups()
    tags = PLATFORMS[archive_name]
    dist_info = f"{name}-{version}.dist-info"
    script = f"{name}-{version}.data/scripts/{BINARY}" + (".exe" if archive_name.endswith(".zip") else "")

    entries: list[tuple[str, bytes, bool]] = []
    with zipfile.ZipFile(pure) as zf:
        for info in zf.infolist():
            if info.filename == f"{dist_info}/RECORD":
                continue
            blob = zf.read(info)
            if info.filename == f"{dist_info}/WHEEL":
                lines = [l for l in blob.decode().splitlines() if not l.startswith(("Tag:", "Root-Is-Purelib:"))]
                lines.append("Root-Is-Purelib: false")
                lines += [f"Tag: py3-none-{t}" for t in tags]
                blob = ("\n".join(lines) + "\n").encode()
            entries.append((info.filename, blob, False))
    entries.append((script, exe, True))
    record = "".join(record_line(p, b) for p, b, _ in entries) + f"{dist_info}/RECORD,,\n"
    entries.append((f"{dist_info}/RECORD", record.encode(), False))

    out.mkdir(parents=True, exist_ok=True)
    wheel = out / f"{name}-{version}-py3-none-{'.'.join(tags)}.whl"
    with zipfile.ZipFile(wheel, "w", zipfile.ZIP_DEFLATED, compresslevel=9) as zf:
        for path, blob, executable in entries:
            # A fixed timestamp keeps a rebuild of the same release byte-identical.
            info = zipfile.ZipInfo(path, date_time=(2020, 1, 1, 0, 0, 0))
            info.compress_type = zipfile.ZIP_DEFLATED
            info.external_attr = (0o100755 if executable else 0o100644) << 16
            zf.writestr(info, blob)
    return wheel


def main(argv: list[str] | None = None) -> int:
    parser = argparse.ArgumentParser(description=__doc__.split("\n\n")[0])
    parser.add_argument("--wheel", type=Path, required=True, help="the pure polign_recall-*-py3-none-any.whl")
    parser.add_argument("--archives", type=Path, required=True, help="directory holding the release archives and checksums.txt")
    parser.add_argument("--out", type=Path, required=True)
    parser.add_argument("--only", action="append", choices=sorted(PLATFORMS), help="build just this archive's wheel (repeatable)")
    args = parser.parse_args(argv)

    sums = checksums((args.archives / "checksums.txt").read_text())
    for name in args.only or PLATFORMS:
        data = (args.archives / name).read_bytes()
        digest = hashlib.sha256(data).hexdigest()
        if sums.get(name) != digest:
            raise SystemExit(f"{name}: sha256 {digest} does not match checksums.txt ({sums.get(name)})")
        wheel = platform_wheel(args.wheel, name, binary(name, data), args.out)
        print(f"{wheel.name}  {wheel.stat().st_size / 1e6:.1f} MB")
    return 0


if __name__ == "__main__":
    raise SystemExit(main())
