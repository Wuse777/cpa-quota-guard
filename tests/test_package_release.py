"""Regression checks for release privacy boundaries and artifact integrity."""
import importlib.util
import json
from pathlib import Path
import tarfile
import tempfile
import unittest

spec = importlib.util.spec_from_file_location("package_release", Path(__file__).parents[1] / "scripts/package_release.py")
release = importlib.util.module_from_spec(spec)
spec.loader.exec_module(release)


class PackageTests(unittest.TestCase):
    def setUp(self):
        self.tmp = tempfile.TemporaryDirectory()
        self.addCleanup(self.tmp.cleanup)
        self.root = Path(self.tmp.name) / "source"
        self.root.mkdir()
        for name in release.ROOT_FILES:
            (self.root / name).write_text("public fixture\n")
        (self.root / "types.go").write_text('package main\nconst version = "0.2.1"\n')
        (self.root / "dist").mkdir()
        self.library = b"\x7fELF\x02\x01" + bytes(12) + b"\x3e\x00" + b"fixture"
        (self.root / "dist/quota-guard.so").write_bytes(self.library)
        (self.root / "dist/quota-guard.h").write_text("/* public header */\n")
        (self.root / "dist/SHA256SUMS").write_text(release.sha256(self.library) + "  dist/quota-guard.so\n")
        self.output = Path(self.tmp.name) / "release"

    def test_private_state_and_generated_reports_are_excluded(self):
        for name in ["private/token.json", "auths/account.json", "tests/run-report.json", "test-results/result.json", "config.yaml"]:
            path = self.root / name
            path.parent.mkdir(parents=True, exist_ok=True)
            path.write_text("private fixture must not ship")
        archive, manifest = release.package(self.root, self.output)
        with tarfile.open(archive) as bundle:
            self.assertTrue(all(b"private fixture" not in bundle.extractfile(m).read() for m in bundle.getmembers()))
            inside = json.load(bundle.extractfile("cpa-quota-guard-0.2.1/MANIFEST.json"))
            self.assertEqual(inside, manifest)
            for item in inside["files"]:
                raw = bundle.extractfile("cpa-quota-guard-0.2.1/" + item["path"]).read()
                self.assertEqual(release.sha256(raw), item["sha256"])

    def test_modified_binary_and_wrong_architecture_fail(self):
        (self.root / "dist/quota-guard.so").write_bytes(self.library + b"modified")
        with self.assertRaisesRegex(ValueError, "SHA256SUMS"):
            release.package(self.root, self.output)
        (self.root / "dist/quota-guard.so").write_bytes(b"MZ" + bytes(40))
        with self.assertRaisesRegex(ValueError, "ELF64"):
            release.package(self.root, self.output)

    def test_symlink_cannot_escape_public_file_allowlist(self):
        outside = Path(self.tmp.name) / "private.txt"
        outside.write_text("private outside repository")
        (self.root / "README.md").unlink()
        (self.root / "README.md").symlink_to(outside)
        with self.assertRaisesRegex(ValueError, "Symlink"):
            release.package(self.root, self.output)

    def test_archive_is_deterministic_for_identical_inputs(self):
        first, _ = release.package(self.root, self.output, epoch=1234)
        raw = first.read_bytes()
        second, _ = release.package(self.root, self.output, epoch=1234)
        self.assertEqual(raw, second.read_bytes())


if __name__ == "__main__":
    unittest.main()
