"""Add the quota-guard session bridge to a verified CPAMC module."""
import hashlib
from pathlib import Path
import re

# Exact upstream asset digest -> (version, auth store, API client).
SUPPORTED_CPAMC = {
    "57558cd023475674634a114824b014d3e5ac161f6f3791e3d86b6adab962a700": ("1.26.5", b"Fm", b"G"),
    "c00ac5577db8feb12d37de6ecb783cf919bb81536902308079f754337d7a7bc5": ("1.28.1", b"hh", b"W"),
}
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
    if digest not in SUPPORTED_CPAMC:
        raise ValueError("Unsupported CPAMC build; expected a verified CPAMC 1.26.5 or 1.28.1 asset. No files changed.")
    if page.count(b"</script>") != 1 or b'<script type="module"' not in page:
        raise ValueError("Expected a single CPAMC module script")
    # Remove only this plugin's previous bridge from the installed page.
    installed = original.replace(own[0], b"", 1) if own else original
    return installed, {"supported": True, "cpamc_version": SUPPORTED_CPAMC[digest][0], "base_sha256": digest,
                       "installed_sha256": hashlib.sha256(original).hexdigest(),
                       "bridge_present": bool(own), "bridge_version": "quota-guard:cpamc-session:v1",
                       "preserved_extensions": preserved}


def panel_diagnostics(page, extensions=None):
    return split_extensions(page, extensions)[1]


def inject_session_bridge(page, extensions=None):
    page, diagnostics = split_extensions(page, extensions)
    _, auth_store, api_client = SUPPORTED_CPAMC[diagnostics["base_sha256"]]
    bridge = (Path(__file__).parent / "web/cpamc-session-bridge.js").read_bytes()
    # The template uses the original build's aliases; adapt only code references.
    bridge = re.sub(rb"\b(Fm|G)(?=\.)", lambda m: auth_store if m[1] == b"Fm" else api_client, bridge)
    return page.replace(b"</script>", START + bridge + END + b"</script>", 1)
