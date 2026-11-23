#!/usr/bin/env python3
"""Build, serve and validate the Boogle documentation site.

Service teams own the Markdown under ``services/<service>/docs/``. This script
copies those pages into ``docs/services/<service>/`` before each build so the
central MkDocs project can navigate to them. The copied tree is generated
output: it is marked with ``.generated-by-scripts-docs-py`` and is not committed.
"""

from __future__ import annotations

import argparse
import os
import re
import shutil
import subprocess
import sys
from pathlib import Path
from typing import Any

ROOT = Path(__file__).resolve().parents[1]
DOCS = ROOT / "docs"
CONFIG = ROOT / "mkdocs.yml"
MARKER = ".generated-by-scripts-docs-py"
SERVICES = ("spider", "engine", "indexer", "ranking")
EXPECTED_PAGES: dict[str, tuple[str, ...]] = {
    "spider": ("index.md", "architecture.md", "implementation.md", "configuration.md"),
    "engine": (
        "index.md",
        "architecture.md",
        "api.md",
        "implementation.md",
        "configuration.md",
    ),
    "indexer": (
        "index.md",
        "architecture.md",
        "indexing-pipeline.md",
        "configuration.md",
    ),
    "ranking": (
        "index.md",
        "architecture.md",
        "algorithms.md",
        "database-functions.md",
        "configuration.md",
    ),
}
COPY_SUFFIXES = {".md", ".png", ".jpg", ".jpeg", ".gif", ".svg", ".webp"}


class PythonName(str):
    """A yaml ``!!python/name:`` value that can round-trip without importing it."""


def _import_yaml() -> Any:
    try:
        import yaml
    except ImportError:
        _fail("PyYAML is required to read the MkDocs configuration")
    return yaml


def load_mkdocs_config() -> dict[str, Any]:
    """Load mkdocs.yml without importing Python objects named in YAML tags."""
    yaml = _import_yaml()

    class ConfigLoader(yaml.SafeLoader):
        pass

    def construct_python_name(loader: Any, suffix: str, node: Any) -> PythonName:
        return PythonName(suffix)

    ConfigLoader.add_multi_constructor(
        "tag:yaml.org,2002:python/name:", construct_python_name
    )
    return yaml.load(CONFIG.read_text(encoding="utf-8"), Loader=ConfigLoader)


def dump_mkdocs_config(config: dict[str, Any], target: Path) -> None:
    """Write a MkDocs configuration, preserving ``!!python/name:`` tags."""
    yaml = _import_yaml()

    class ConfigDumper(yaml.SafeDumper):
        pass

    def represent_python_name(dumper: Any, data: PythonName) -> Any:
        return dumper.represent_scalar("tag:yaml.org,2002:python/name:", str(data))

    ConfigDumper.add_representer(PythonName, represent_python_name)
    rendered = yaml.dump(config, Dumper=ConfigDumper, sort_keys=False, allow_unicode=True)
    # SafeDumper quotes the value after a python-name tag; MkDocs expects the
    # compact ``!!python/name:module.attr`` form used in mkdocs.yml.
    rendered = re.sub(r"!!python/name: '([^']+)'", r"!!python/name:\1", rendered)
    target.write_text(rendered, encoding="utf-8")


def _fail(message: str) -> None:
    print(f"error: {message}", file=sys.stderr)
    raise SystemExit(1)


def _copy_managed_tree(source: Path, target: Path) -> int:
    """Copy source into target, removing files that no longer exist upstream."""
    if not source.is_dir():
        return 0
    if target.exists() and not (target / MARKER).is_dir():
        _fail(
            f"{target} exists but was not created by this script; "
            "refusing to overwrite files that may be hand-written"
        )
    target.mkdir(parents=True, exist_ok=True)
    (target / MARKER).mkdir(exist_ok=True)

    wanted: dict[Path, Path] = {}
    for path in source.rglob("*"):
        if not path.is_file() or path.suffix.lower() not in COPY_SUFFIXES:
            continue
        if any(part.startswith(".") for part in path.relative_to(source).parts):
            continue
        wanted[path.relative_to(source)] = path

    for path in sorted(target.rglob("*")):
        if not path.is_file():
            continue
        relative = path.relative_to(target)
        if relative.parts[0] == MARKER:
            continue
        if relative not in wanted:
            path.unlink()

    for relative, source_file in sorted(wanted.items()):
        target_file = target / relative
        target_file.parent.mkdir(parents=True, exist_ok=True)
        if not target_file.exists() or target_file.read_bytes() != source_file.read_bytes():
            shutil.copy2(source_file, target_file)
    return len(wanted)


def aggregate(service: str | None = None) -> None:
    """Copy service-owned documentation into the central docs tree."""
    selected = SERVICES if service is None else (service,)
    for name in selected:
        if name not in SERVICES:
            _fail(f"unknown service {name!r}; expected one of {', '.join(SERVICES)}")
        source = ROOT / "services" / name / "docs"
        if not source.is_dir():
            _fail(f"missing service documentation directory: {source}")
        missing = [
            page for page in EXPECTED_PAGES[name] if not (source / page).is_file()
        ]
        if missing:
            _fail(f"{source} is missing: {', '.join(missing)}")
        count = _copy_managed_tree(source, DOCS / "services" / name)
        print(f"aggregated {count} file(s) from services/{name}/docs")

    asset_count = _copy_managed_tree(ROOT / "assets", DOCS / "assets")
    print(f"aggregated {asset_count} asset(s) from assets/")


def mkdocs_executable() -> str:
    override = os.environ.get("MKDOCS")
    if override:
        return override
    local = ROOT / ".venv-docs" / "bin" / "mkdocs"
    if local.is_file() and os.access(local, os.X_OK):
        return str(local)
    found = shutil.which("mkdocs")
    if found:
        return found
    _fail(
        "mkdocs not found. Run 'just docs-install' or "
        "'python3 -m venv .venv-docs && .venv-docs/bin/pip install -r docs/requirements.txt'."
    )
    raise AssertionError("unreachable")


def run_mkdocs(arguments: list[str]) -> None:
    command = [mkdocs_executable(), *arguments]
    print("==>", " ".join(command))
    subprocess.run(command, cwd=ROOT, check=True)


def build(arguments: argparse.Namespace) -> None:
    aggregate()
    run_mkdocs(
        [
            "build",
            "--strict",
            "--config-file",
            str(CONFIG),
            "--site-dir",
            str(ROOT / arguments.site_dir),
        ]
    )


def serve(arguments: argparse.Namespace) -> None:
    aggregate()
    # `mkdocs serve` does not accept --site-dir; it builds into its own
    # temporary directory and ignores the configured site_dir.
    run_mkdocs(
        [
            "serve",
            "--strict",
            "--config-file",
            str(CONFIG),
            "--dev-addr",
            arguments.address,
        ]
    )


def service_docs(arguments: argparse.Namespace) -> None:
    """Build one service's documentation as a small standalone site."""
    name = arguments.service
    if name not in SERVICES:
        _fail(f"unknown service {name!r}; expected one of {', '.join(SERVICES)}")
    aggregate(service=name)

    config: dict[str, Any] = load_mkdocs_config()
    service_nav: Any = None
    for entry in config["nav"]:
        if isinstance(entry, dict) and "Services" in entry:
            for child in entry["Services"]:
                if isinstance(child, dict) and name.title() in child:
                    service_nav = child[name.title()]
    if service_nav is None:
        _fail(f"mkdocs.yml has no navigation entry for service {name!r}")

    config["site_name"] = f"Boogle — {name.title()} service"
    config["site_dir"] = str(ROOT / "site-services" / name)
    config["docs_dir"] = str(DOCS)
    config["nav"] = [{"Home": f"services/{name}/index.md"}, {"Pages": service_nav}]
    # A standalone site contains only this service's pages. Excluding other
    # Markdown keeps central pages out of the build as well as out of the nav.
    config["exclude_docs"] = f"*.md\n!services/{name}/*.md"
    config["validation"]["nav"]["omitted_files"] = "ignore"
    config.pop("hooks", None)

    build_dir = ROOT / ".docs-build"
    build_dir.mkdir(exist_ok=True)
    config_file = build_dir / f"mkdocs-{name}.yml"
    dump_mkdocs_config(config, config_file)
    run_mkdocs(["build", "--strict", "--config-file", str(config_file)])


def nav_files(nav: Any) -> set[str]:
    found: set[str] = set()
    if isinstance(nav, list):
        for item in nav:
            found |= nav_files(item)
    elif isinstance(nav, dict):
        for value in nav.values():
            if isinstance(value, str):
                found.add(value)
            else:
                found |= nav_files(value)
    elif isinstance(nav, str):
        found.add(nav)
    return found


def check(arguments: argparse.Namespace) -> None:
    """Aggregate and run strict validation, including nav completeness."""
    aggregate()
    config: dict[str, Any] = load_mkdocs_config()

    linked = nav_files(config.get("nav", []))
    not_linked = []
    for name in SERVICES:
        for path in sorted((DOCS / "services" / name).rglob("*.md")):
            relative = path.relative_to(DOCS).as_posix()
            if relative not in linked:
                not_linked.append(relative)
    if not_linked:
        _fail("service pages missing from mkdocs.yml nav: " + ", ".join(not_linked))

    missing_nav = [path for path in sorted(linked) if not (DOCS / path).is_file()]
    if missing_nav:
        _fail("nav points to missing files: " + ", ".join(missing_nav))

    target = ROOT / ".docs-build" / "check-site"
    if target.exists():
        shutil.rmtree(target)
    run_mkdocs(
        ["build", "--strict", "--config-file", str(CONFIG), "--site-dir", str(target)]
    )
    pages = {path.resolve() for path in DOCS.rglob("*.md") if MARKER not in path.parts}
    print(f"documentation check passed ({len(pages)} Markdown source pages)")


def install(arguments: argparse.Namespace) -> None:
    venv = ROOT / ".venv-docs"
    if not venv.exists():
        subprocess.run([sys.executable, "-m", "venv", str(venv)], cwd=ROOT, check=True)
    subprocess.run(
        [str(venv / "bin" / "pip"), "install", "-r", str(DOCS / "requirements.txt")],
        cwd=ROOT,
        check=True,
    )


def main() -> None:
    parser = argparse.ArgumentParser(description=__doc__)
    subparsers = parser.add_subparsers(dest="command", required=True)

    p_aggregate = subparsers.add_parser("aggregate", help="copy service docs into docs/")
    p_aggregate.add_argument("--service", choices=SERVICES)
    p_aggregate.set_defaults(function=lambda args: aggregate(args.service))

    p_build = subparsers.add_parser("build", help="build the complete site")
    p_build.add_argument("--site-dir", default="site")
    p_build.set_defaults(function=build)

    p_serve = subparsers.add_parser("serve", help="serve the complete site")
    p_serve.add_argument("--address", default="127.0.0.1:8000")
    p_serve.set_defaults(function=serve)

    p_service = subparsers.add_parser("service", help="build one service's docs")
    p_service.add_argument("service", choices=SERVICES)
    p_service.set_defaults(function=service_docs)

    p_check = subparsers.add_parser("check", help="validate nav and build in strict mode")
    p_check.set_defaults(function=check)

    p_install = subparsers.add_parser("install", help="create .venv-docs and install deps")
    p_install.set_defaults(function=install)

    arguments = parser.parse_args()
    arguments.function(arguments)


if __name__ == "__main__":
    main()
