"""Add the quota-guard session bridge to the verified CPAMC 1.26.5 module."""
import hashlib
from pathlib import Path

SUPPORTED_CPAMC_SHA256 = {"57558cd023475674634a114824b014d3e5ac161f6f3791e3d86b6adab962a700"}
START = b"\n/* quota-guard-session-bridge:start */\n"
END = b"\n/* quota-guard-session-bridge:end */\n"


def inject_session_bridge(page):
    if START in page:
        before, _, tail = page.partition(START)
        _, end, after = tail.partition(END)
        if not end:
            raise ValueError("Existing CPAMC session bridge is incomplete")
        page = before + after
    if hashlib.sha256(page).hexdigest() not in SUPPORTED_CPAMC_SHA256:
        raise ValueError("Unsupported CPAMC build; expected verified CPAMC 1.26.5. No files changed.")
    if page.count(b"</script>") != 1 or b'<script type="module"' not in page:
        raise ValueError("Expected a single CPAMC module script")
    bridge = (Path(__file__).parent / "web/cpamc-session-bridge.js").read_bytes()
    return page.replace(b"</script>", START + bridge + END + b"</script>", 1)
