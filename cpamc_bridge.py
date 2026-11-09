"""Add the quota-guard session bridge to the verified CPAMC 1.26.5 module."""
import hashlib
from pathlib import Path
import re

SUPPORTED_CPAMC_SHA256 = {"57558cd023475674634a114824b014d3e5ac161f6f3791e3d86b6adab962a700"}
START = b"\n/* quota-guard-session-bridge:start */\n"
END = b"\n/* quota-guard-session-bridge:end */\n"


def split_extensions(page, extensions=None):
    """Validate the original build while preserving explicitly trusted add-ons.

    Other scripts are accepted only by exact marked-block SHA256 declarations;
    marker names alone never bypass the verified original HTML fingerprint.
    """
    original = page
    own = []
    if page.count(START) > 1 or page.count(END) > 1:
        raise ValueError("Duplicate CPAMC session bridge")
    if START in page or END in page:
        before, start, tail = page.partition(START)
        content, end, after = tail.partition(END)
        if not start or not end:
            raise ValueError("Existing CPAMC session bridge is incomplete")
        own.append(START + content + END)
        page = before + after
    preserved = []
    for item in (extensions or {}).get("extensions", []):
        marker = item.get("marker", "")
        if not re.fullmatch(r"[A-Za-z0-9][A-Za-z0-9_-]{0,79}", marker) or marker == "quota-guard-session-bridge":
            raise ValueError("Invalid external panel extension marker")
        start = ("\n/* " + marker + ":start */\n").encode()
        end = ("\n/* " + marker + ":end */\n").encode()
        if page.count(start) != 1 or page.count(end) != 1:
            raise ValueError("Missing, duplicate or incomplete external panel extension: " + marker)
        before, _, tail = page.partition(start)
        content, _, after = tail.partition(end)
        block = start + content + end
        if hashlib.sha256(block).hexdigest() != item.get("sha256"):
            raise ValueError("External panel extension SHA256 mismatch: " + marker)
        preserved.append({"marker": marker, "sha256": item["sha256"]})
        page = before + after
    digest = hashlib.sha256(page).hexdigest()
    if digest not in SUPPORTED_CPAMC_SHA256:
        raise ValueError("Unsupported CPAMC build; expected verified CPAMC 1.26.5. No files changed.")
    if page.count(b"</script>") != 1 or b'<script type="module"' not in page:
        raise ValueError("Expected a single CPAMC module script")
    # Remove only this plugin's previous bridge from the installed page.
    installed = original.replace(own[0], b"", 1) if own else original
    return installed, {"supported": True, "cpamc_version": "1.26.5", "base_sha256": digest,
                       "installed_sha256": hashlib.sha256(original).hexdigest(),
                       "bridge_present": bool(own), "bridge_version": "quota-guard:cpamc-session:v1",
                       "preserved_extensions": preserved}


def panel_diagnostics(page, extensions=None):
    return split_extensions(page, extensions)[1]


def inject_session_bridge(page, extensions=None):
    page, _ = split_extensions(page, extensions)
    bridge = (Path(__file__).parent / "web/cpamc-session-bridge.js").read_bytes()
    return page.replace(b"</script>", START + bridge + END + b"</script>", 1)
