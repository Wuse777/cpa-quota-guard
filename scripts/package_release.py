"""Build a Linux amd64 release from an explicit public-file allowlist."""
import argparse
import gzip
import hashlib
import io
import json
import os
from pathlib import Path
import re
import tarfile

ROOT_FILES = {
    "README.md", "README.en.md", "LICENSE", "NOTICE", "CHANGELOG.md",
    "SECURITY.md", "CONTRIBUTING.md", "go.mod", "go.sum", "build.sh",
    "requirements.txt", "install.py", "cpamc_bridge.py", "config.example.yaml",
    "policy.example.json", "policy.safe-start.example.json",
    "package.json", "package-lock.json",
}
DIRECTORIES = {
    "web": {".html", ".css", ".js"},
    "licenses": {".txt"},
    "docs": {".md", ".png"},
    "scripts": {".py", ".sh"},
    "tests": {".py", ".mjs", ".c"},
}


def sha256(raw):
    return hashlib.sha256(raw).hexdigest()


def public_files(root):
    selected = {root / name for name in ROOT_FILES}
    selected.update(root.glob("*.go"))
    for directory, extensions in DIRECTORIES.items():
        selected.update(p for p in (root / directory).rglob("*")
                        if p.suffix in extensions and "__pycache__" not in p.parts)
    selected.update(root / "dist" / name for name in ["quota-guard.so", "quota-guard.h", "SHA256SUMS", "BUILD.json"])
    for path in selected:
        if not path.is_file():
            raise ValueError(f"Missing release input: {path.relative_to(root)}")
        relative = path.relative_to(root)
        if any((root / Path(*relative.parts[:i])).is_symlink() for i in range(1, len(relative.parts) + 1)):
            raise ValueError(f"Symlink not allowed in a release: {relative}")
    return sorted(selected, key=lambda p: p.relative_to(root).as_posix())


def package(root, output, epoch=0):
    root, output = Path(root).resolve(), Path(output).resolve()
    match = re.search(r'^const version = "([0-9]+\.[0-9]+\.[0-9]+)"$',
                      (root / "types.go").read_text(), re.MULTILINE)
    if not match:
        raise ValueError("Cannot read plugin version from types.go")
    version = match.group(1)
    library = (root / "dist/quota-guard.so").read_bytes()
    # ELF64, little endian, x86-64. A Windows DLL or ARM library must not be mislabeled.
    if len(library) < 20 or library[:6] != b"\x7fELF\x02\x01" or library[18:20] != b"\x3e\x00":
        raise ValueError("Expected an ELF64 little-endian x86-64 shared library")
    checksums = (root / "dist/SHA256SUMS").read_text().splitlines()
    if f"{sha256(library)}  dist/quota-guard.so" not in checksums:
        raise ValueError("dist/SHA256SUMS does not match the shared library")
    build = json.loads((root / "dist/BUILD.json").read_text())
    if build.get("version") != version or build.get("sha256") != sha256(library):
        raise ValueError("BUILD.json does not match the version/shared library")
    contents = {p.relative_to(root).as_posix(): p.read_bytes() for p in public_files(root)}
    manifest = {
        "schema_version": 1, "name": "cpa-quota-guard", "version": version,
        "target": "linux-amd64-glibc", "verified_cpa": "8.0.35", "verified_cpamc": "1.26.5",
        "build": build,
        "files": [{"path": name, "size": len(raw), "sha256": sha256(raw)} for name, raw in contents.items()],
    }
    contents["MANIFEST.json"] = (json.dumps(manifest, indent=2) + "\n").encode()
    output.mkdir(parents=True, exist_ok=True)
    archive = output / f"cpa-quota-guard_{version}_linux_amd64.tar.gz"
    with archive.open("wb") as fileobj, gzip.GzipFile(filename="", mode="wb", fileobj=fileobj, mtime=epoch) as compressed:
        with tarfile.open(fileobj=compressed, mode="w") as bundle:
            for name, raw in sorted(contents.items()):
                info = tarfile.TarInfo(f"cpa-quota-guard-{version}/{name}")
                info.size, info.mtime = len(raw), epoch
                info.mode = 0o755 if name.endswith(".sh") or name == "dist/quota-guard.so" else 0o644
                bundle.addfile(info, io.BytesIO(raw))
    digest = sha256(archive.read_bytes())
    (output / "SHA256SUMS").write_text(f"{digest}  {archive.name}\n")
    return archive, manifest


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--output-dir", type=Path, default=Path("release"))
    args = parser.parse_args()
    archive, manifest = package(Path(__file__).resolve().parents[1], args.output_dir,
                                int(os.environ.get("SOURCE_DATE_EPOCH", "0")))
    print(json.dumps({"archive": str(archive), "version": manifest["version"],
                      "files": len(manifest["files"]), "sha256": sha256(archive.read_bytes())}, indent=2))


if __name__ == "__main__":
    main()
