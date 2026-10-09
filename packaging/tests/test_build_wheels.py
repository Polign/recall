"""build_wheels.py against a synthetic pure wheel and release archives: no network, no real binaries."""

from __future__ import annotations

import hashlib
import importlib.util
import io
import tarfile
import tempfile
import unittest
import zipfile
from pathlib import Path

ROOT = Path(__file__).resolve().parents[1]
spec = importlib.util.spec_from_file_location("build_wheels", ROOT / "build_wheels.py")
build_wheels = importlib.util.module_from_spec(spec)
spec.loader.exec_module(build_wheels)

FILES = {"recall": b"recall-binary", "README.md": b"unused", "LICENSE": b"license text"}
DIST = "polign_recall-0.12.0.dist-info"


def pure_wheel(directory: Path) -> Path:
    """A stand-in for what `python -m build` makes."""
    path = directory / "polign_recall-0.12.0-py3-none-any.whl"
    with zipfile.ZipFile(path, "w") as zf:
        zf.writestr("polign_recall/__init__.py", '__version__ = "0.12.0"\n')
        zf.writestr("polign_recall/client.py", "# client\n")
        zf.writestr(f"{DIST}/METADATA", "Metadata-Version: 2.4\nName: polign-recall\nVersion: 0.12.0\n")
        zf.writestr(f"{DIST}/WHEEL", "Wheel-Version: 1.0\nGenerator: setuptools\nRoot-Is-Purelib: true\nTag: py3-none-any\n")
        zf.writestr(f"{DIST}/RECORD", "stale\n")
    return path


def release(directory: Path, skip: str | None = None) -> Path:
    """A fake release directory: one tar.gz, one zip, and checksums.txt."""
    buf = io.BytesIO()
    with tarfile.open(fileobj=buf, mode="w:gz") as tf:
        for name, blob in FILES.items():
            if name != skip:
                info = tarfile.TarInfo(name)
                info.size = len(blob)
                tf.addfile(info, io.BytesIO(blob))
    (directory / "recall_linux_amd64.tar.gz").write_bytes(buf.getvalue())
    buf = io.BytesIO()
    with zipfile.ZipFile(buf, "w") as zf:
        for name, blob in FILES.items():
            zf.writestr(name if name != "recall" else "recall.exe", blob)
    (directory / "recall_windows_amd64.zip").write_bytes(buf.getvalue())
    (directory / "checksums.txt").write_text("".join(
        f"{hashlib.sha256(p.read_bytes()).hexdigest()}  {p.name}\n" for p in sorted(directory.glob("recall_*"))))
    return directory


class BuildWheels(unittest.TestCase):
    def setUp(self) -> None:
        self.tmp = Path(tempfile.mkdtemp())
        self.archives = self.tmp / "release"
        self.archives.mkdir()
        self.out = self.tmp / "dist"
        self.pure = pure_wheel(self.tmp)

    def build(self, *only: str) -> list[Path]:
        args = ["--wheel", str(self.pure), "--archives", str(self.archives), "--out", str(self.out)]
        for name in only:
            args += ["--only", name]
        build_wheels.main(args)
        return sorted(self.out.glob("*.whl"))

    def test_platform_wheel_is_the_client_plus_recall(self) -> None:
        release(self.archives)
        wheels = self.build("recall_linux_amd64.tar.gz", "recall_windows_amd64.zip")
        self.assertEqual(len(wheels), 2)
        linux = next(w for w in wheels if "manylinux" in w.name)
        with zipfile.ZipFile(linux) as zf:
            names = zf.namelist()
            self.assertIn("polign_recall/client.py", names)
            script = "polign_recall-0.12.0.data/scripts/recall"
            self.assertEqual(zf.read(script), b"recall-binary")
            self.assertTrue((zf.getinfo(script).external_attr >> 16) & 0o111, "recall must be executable")
            wheel = zf.read(f"{DIST}/WHEEL").decode()
            self.assertIn("Root-Is-Purelib: false", wheel)
            self.assertNotIn("py3-none-any", wheel)
            self.assertIn("Tag: py3-none-manylinux2014_x86_64", wheel)
            record = zf.read(f"{DIST}/RECORD").decode()
            self.assertIn(script + ",sha256=", record)
            self.assertNotIn("stale", record)
        windows = next(w for w in wheels if "win_amd64" in w.name)
        with zipfile.ZipFile(windows) as zf:
            self.assertIn("polign_recall-0.12.0.data/scripts/recall.exe", zf.namelist())

    def test_checksum_mismatch_is_refused(self) -> None:
        release(self.archives)
        (self.archives / "recall_linux_amd64.tar.gz").write_bytes(b"tampered")
        with self.assertRaises(SystemExit):
            self.build("recall_linux_amd64.tar.gz")

    def test_archive_without_the_binary_is_refused(self) -> None:
        release(self.archives, skip="recall")
        with self.assertRaises(SystemExit):
            self.build("recall_linux_amd64.tar.gz")


if __name__ == "__main__":
    unittest.main()
