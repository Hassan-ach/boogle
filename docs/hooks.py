"""MkDocs hook: refresh aggregated service docs before each build/serve."""

from __future__ import annotations

import subprocess
import sys
from pathlib import Path


def on_pre_build(config, **kwargs) -> None:
    root = Path(config["docs_dir"]).resolve().parent
    subprocess.run(
        [sys.executable, str(root / "scripts" / "docs.py"), "aggregate"],
        cwd=root,
        check=True,
    )
