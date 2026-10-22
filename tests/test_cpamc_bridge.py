import hashlib
import unittest
from unittest.mock import patch

import cpamc_bridge


class PanelBridgeTests(unittest.TestCase):
    def test_verified_build_patches_once_and_upgrades_idempotently(self):
        original = b'<html><script type="module">const G={},Fm={};</script></html>'
        with patch.object(cpamc_bridge, "SUPPORTED_CPAMC_SHA256", {hashlib.sha256(original).hexdigest()}):
            patched = cpamc_bridge.inject_session_bridge(original)
            self.assertIn(b"quota-guard:cpamc-session:v1", patched)
            self.assertEqual(patched.count(cpamc_bridge.START), 1)
            self.assertEqual(cpamc_bridge.inject_session_bridge(patched), patched)

    def test_unknown_build_and_corrupt_existing_bridge_fail_before_mutation(self):
        with self.assertRaisesRegex(ValueError, "Unsupported CPAMC"):
            cpamc_bridge.inject_session_bridge(b"unknown HTML")
        with self.assertRaisesRegex(ValueError, "incomplete"):
            cpamc_bridge.inject_session_bridge(b"original" + cpamc_bridge.START + b"broken")


if __name__ == "__main__":
    unittest.main()
