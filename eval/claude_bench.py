#!/usr/bin/env python3
"""Benchmark browser MCP servers inside real headless Claude Code sessions.

Same task prompt, same model, one MCP server per run (--strict-mcp-config).
Reports wall time, turns, Claude cost and pass/fail per task and server.

usage: claude_bench.py --config brojev=path.json --config playwright=path.json [--repeat n] [--out results.json]
"""
import argparse
import json
import re
import subprocess
import time

TASKS = [
    {
        "name": "books-price",
        "prompt": "Open https://books.toscrape.com/ and find the book 'A Light in the Attic'. Reply with only its price and stock availability.",
        "check": [r"51\.77", r"22"],
    },
    {
        "name": "ti-login",
        "prompt": "Go to https://the-internet.herokuapp.com/login and log in with username tomsmith and password SuperSecretPassword! . Reply with only the exact success message shown after login.",
        "check": [r"logged into a secure area"],
    },
    {
        "name": "ti-confirm-cancel",
        "prompt": "Go to https://the-internet.herokuapp.com/javascript_alerts, click the 'Click for JS Confirm' button and cancel the dialog. Reply with only the result text the page shows.",
        "check": [r"You clicked: Cancel"],
    },
    {
        "name": "wiki-godel-year",
        "prompt": "Starting at https://en.wikipedia.org/wiki/Main_Page, use the site search to open the article about Gödel's incompleteness theorems. Reply with only the year Gödel published them, as stated in the article's lead.",
        "check": [r"1931"],
    },
]


def run(server, config, task, timeout):
    cmd = [
        "claude", "-p", "--strict-mcp-config", "--mcp-config", config,
        "--allowedTools", f"mcp__{server}", "--output-format", "json", task["prompt"],
    ]
    start = time.time()
    try:
        p = subprocess.run(cmd, capture_output=True, text=True, timeout=timeout, stdin=subprocess.DEVNULL)
        wall = time.time() - start
        d = json.loads(p.stdout)
    except subprocess.TimeoutExpired:
        return {"server": server, "task": task["name"], "pass": False, "error": "timeout", "wall_s": timeout}
    except json.JSONDecodeError:
        return {"server": server, "task": task["name"], "pass": False, "error": p.stderr[-300:], "wall_s": time.time() - start}
    result = d.get("result") or ""
    ok = all(re.search(c, result, re.I) for c in task["check"])
    return {
        "server": server, "task": task["name"], "pass": ok, "wall_s": round(wall, 1),
        "turns": d.get("num_turns"), "cost_usd": round(d.get("total_cost_usd") or 0, 4),
        "result": result[:200],
    }


def main():
    ap = argparse.ArgumentParser()
    ap.add_argument("--config", action="append", required=True, help="server=path/to/mcp.json")
    ap.add_argument("--repeat", type=int, default=1)
    ap.add_argument("--only")
    ap.add_argument("--timeout", type=int, default=300)
    ap.add_argument("--out")
    a = ap.parse_args()
    servers = [c.split("=", 1) for c in a.config]
    rows = []
    print(f"{'task':20} {'server':11} {'pass':5} {'wall s':>7} {'turns':>5} {'cost $':>7}  result")
    for task in TASKS:
        if a.only and a.only not in task["name"]:
            continue
        for _ in range(a.repeat):
            for server, config in servers:  # interleave servers to share network conditions
                r = run(server, config, task, a.timeout)
                rows.append(r)
                print(f"{r['task']:20} {r['server']:11} {str(r['pass']):5} {r.get('wall_s', 0):7} {str(r.get('turns', '')):>5} {r.get('cost_usd', 0):7}  {r.get('result', r.get('error', ''))[:60]!r}")
    print()
    for server, _ in servers:
        rs = [r for r in rows if r["server"] == server]
        if not rs:
            continue
        passed = sum(r["pass"] for r in rs)
        wall = sum(r.get("wall_s", 0) for r in rs) / len(rs)
        cost = sum(r.get("cost_usd", 0) for r in rs)
        turns = sum(r.get("turns") or 0 for r in rs) / len(rs)
        print(f"{server:11} passed {passed}/{len(rs)}  mean wall {wall:.1f}s  mean turns {turns:.1f}  total Claude cost ${cost:.3f}")
    if a.out:
        json.dump(rows, open(a.out, "w"), indent=2)


if __name__ == "__main__":
    main()
