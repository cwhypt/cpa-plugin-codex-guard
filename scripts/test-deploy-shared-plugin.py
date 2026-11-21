"""Regression checks for config inode preservation and concurrent-write rejection."""
import importlib.util
from pathlib import Path
import tempfile
import unittest

spec = importlib.util.spec_from_file_location("deploy_guard", Path(__file__).with_name("deploy-shared-plugin.py"))
deploy = importlib.util.module_from_spec(spec)
spec.loader.exec_module(deploy)


class ConfigWriteTests(unittest.TestCase):
    def test_preserves_watched_inode(self):
        with tempfile.TemporaryDirectory() as directory:
            path = Path(directory)/"config.yaml"
            path.write_bytes(b"old config")
            inode = path.stat().st_ino
            deploy.write_watched_config(path, b"old config", b"new")
            self.assertEqual(path.stat().st_ino, inode)
            self.assertEqual(path.read_bytes(), b"new")

    def test_rejects_concurrent_edit_without_overwriting(self):
        with tempfile.TemporaryDirectory() as directory:
            path = Path(directory)/"config.yaml"
            path.write_bytes(b"another editor")
            with self.assertRaisesRegex(AssertionError, "changed concurrently"):
                deploy.write_watched_config(path, b"old config", b"new")
            self.assertEqual(path.read_bytes(), b"another editor")


if __name__ == "__main__":
    unittest.main()
