"""server.json is what the MCP Registry publishes; the registry refuses what breaks its limits."""

import json
import pathlib
import unittest

ROOT = pathlib.Path(__file__).resolve().parents[2]


class ServerJSON(unittest.TestCase):
    def test_description_fits_the_registry(self):
        doc = json.loads((ROOT / "server.json").read_text())
        # The registry answers 422 "expected length <= 100" on a longer one.
        self.assertLessEqual(len(doc["description"]), 100)

    def test_version_matches_the_python_package(self):
        doc = json.loads((ROOT / "server.json").read_text())
        pyproject = (ROOT / "python" / "pyproject.toml").read_text()
        self.assertIn(f'version = "{doc["version"]}"', pyproject)


if __name__ == "__main__":
    unittest.main()
