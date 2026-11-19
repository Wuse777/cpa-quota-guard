import hashlib
import unittest
from unittest.mock import patch

import cpamc_bridge


class PanelBridgeTests(unittest.TestCase):
    def test_external_extension_requires_exact_declaration_and_survives_upgrade(self):
        original = b'<html><script type="module">const G={},Fm={};</script></html>'
        extension = b'\n/* example-sso:start */\nconst sso="fixture";\n/* example-sso:end */\n'
        extended = original.replace(b'</script>', extension + b'</script>')
        declarations = {"extensions": [{"marker": "example-sso", "sha256": hashlib.sha256(extension).hexdigest()}]}
        with patch.object(cpamc_bridge, "SUPPORTED_CPAMC", {hashlib.sha256(original).hexdigest(): ("1.26.5", b"Fm", b"G")}):
            with self.assertRaisesRegex(ValueError, "Unsupported"):
                cpamc_bridge.inject_session_bridge(extended)
            patched = cpamc_bridge.inject_session_bridge(extended, declarations)
            self.assertIn(extension, patched)
            self.assertEqual(cpamc_bridge.inject_session_bridge(patched, declarations), patched)
            self.assertEqual(cpamc_bridge.panel_diagnostics(patched, declarations)["base_sha256"], hashlib.sha256(original).hexdigest())
            with self.assertRaisesRegex(ValueError, "SHA256 mismatch"):
                cpamc_bridge.inject_session_bridge(extended.replace(b'fixture', b'changed'), declarations)

    def test_arbitrary_base_changes_and_duplicate_markers_are_rejected(self):
        original = b'<html><script type="module">fixture</script></html>'
        with patch.object(cpamc_bridge, "SUPPORTED_CPAMC", {hashlib.sha256(original).hexdigest(): ("1.26.5", b"Fm", b"G")}):
            with self.assertRaisesRegex(ValueError, "Unsupported"):
                cpamc_bridge.inject_session_bridge(original.replace(b'fixture', b'changed'))
            patched = cpamc_bridge.inject_session_bridge(original)
            with self.assertRaisesRegex(ValueError, "Duplicate"):
                cpamc_bridge.inject_session_bridge(patched + cpamc_bridge.START + cpamc_bridge.END)

    def test_verified_build_patches_once_and_upgrades_idempotently(self):
        original = b'<html><script type="module">const G={},Fm={};</script></html>'
        with patch.object(cpamc_bridge, "SUPPORTED_CPAMC", {hashlib.sha256(original).hexdigest(): ("1.26.5", b"Fm", b"G")}):
            patched = cpamc_bridge.inject_session_bridge(original)
            self.assertIn(b"quota-guard:cpamc-session:v1", patched)
            self.assertEqual(patched.count(cpamc_bridge.START), 1)
            self.assertEqual(cpamc_bridge.inject_session_bridge(patched), patched)

    def test_new_build_uses_its_own_auth_and_client_symbols(self):
        original = b'<html><script type="module">const W={},hh={};</script></html>'
        builds = {hashlib.sha256(original).hexdigest(): ("1.28.1", b"hh", b"W")}
        with patch.object(cpamc_bridge, "SUPPORTED_CPAMC", builds):
            patched = cpamc_bridge.inject_session_bridge(original)
            self.assertIn(b"hh.getState()", patched)
            self.assertIn(b"W.instance.request", patched)
            self.assertIn(b"W.getConnectionRevision()", patched)
            self.assertNotIn(b"Fm.getState()", patched)
            self.assertNotIn(b"G.instance.request", patched)
            self.assertEqual(cpamc_bridge.panel_diagnostics(patched)["cpamc_version"], "1.28.1")
            self.assertEqual(cpamc_bridge.inject_session_bridge(patched), patched)

    def test_unknown_build_and_corrupt_existing_bridge_fail_before_mutation(self):
        with self.assertRaisesRegex(ValueError, "Unsupported CPAMC"):
            cpamc_bridge.inject_session_bridge(b"unknown HTML")
        with self.assertRaisesRegex(ValueError, "incomplete"):
            cpamc_bridge.inject_session_bridge(b"original" + cpamc_bridge.START + b"broken")


if __name__ == "__main__":
    unittest.main()
