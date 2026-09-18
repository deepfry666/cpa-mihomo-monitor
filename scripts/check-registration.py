#!/usr/bin/env python3
"""Check that CPA registered the Mihomo Monitor entry plugin and its menu."""

import argparse
import json
from pathlib import Path
import urllib.request


def main() -> None:
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--base", default="http://127.0.0.1:8317")
    parser.add_argument("--key-file", required=True)
    args = parser.parse_args()

    admin_key = Path(args.key_file).read_text().strip()
    request = urllib.request.Request(
        args.base.rstrip("/") + "/v0/management/plugins",
        headers={
            "Authorization": "Bearer " + admin_key,
            "Accept": "application/json",
        },
    )
    with urllib.request.urlopen(request, timeout=20) as response:
        payload = json.loads(response.read())

    plugin = next(
        (item for item in payload.get("plugins", []) if item.get("id") == "mihomo-monitor"),
        None,
    )
    result = {
        "found": plugin is not None,
        "enabled": bool(plugin and plugin.get("enabled")),
        "registered": bool(plugin and plugin.get("registered")),
        "effective_enabled": bool(plugin and plugin.get("effective_enabled")),
        "menus": plugin.get("menus", []) if plugin else [],
    }
    result["expected_menu"] = any(
        menu.get("menu") == "Mihomo 监控"
        and menu.get("path") == "/v0/resource/plugins/mihomo-monitor/dashboard"
        for menu in result["menus"]
    )
    print(json.dumps(result, ensure_ascii=False, sort_keys=True))
    if not (
        result["found"]
        and result["enabled"]
        and result["registered"]
        and result["effective_enabled"]
        and result["expected_menu"]
    ):
        raise SystemExit(1)


if __name__ == "__main__":
    main()
