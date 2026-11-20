#!/usr/bin/env python3
"""Aggregate downstream GPT request lengths without printing conversation content."""
import argparse
import collections
import hashlib
import json
import math
from pathlib import Path
import re
import statistics
import tempfile


def text_content(content):
    if isinstance(content, str):
        return content.strip()
    if isinstance(content, list):
        return "\n".join(p["text"].strip() for p in content if isinstance(p, dict)
                         and p.get("type") in ("text", "input_text", "output_text")
                         and isinstance(p.get("text"), str))
    return ""


def metrics(payload):
    items = payload.get("input", payload.get("messages", []))
    users, context = [], 0
    if isinstance(items, str):
        users = [items.strip()]
        context = len(items.strip())
    elif isinstance(items, list):
        for item in items:
            if not isinstance(item, dict) or item.get("role") in ("system", "developer"):
                continue
            text = text_content(item.get("content"))
            if item.get("role") == "user" and text:
                users.append(text)
            if item.get("role") in ("user", "assistant", "tool", "function"):
                context += len(text)
            if item.get("type") == "function_call_output" and isinstance(item.get("output"), str):
                context += len(item["output"].strip())
    first = users[0] if users else ""
    normalized = re.sub(r"\d+", "#", re.sub(r"\s+", " ", first.lower())).strip()
    return dict(first_user_chars=len(first), context_chars=context,
                template=hashlib.sha256(normalized.encode()).hexdigest()[:12])


def quantiles(values):
    values = sorted(values)
    if not values:
        return {}
    return {name: values[round((len(values)-1)*p)] for name, p in
            (("min", 0), ("p25", .25), ("median", .5), ("p75", .75), ("p90", .9), ("max", 1))}


def clusters(values):
    if not values:
        return []
    points = [math.log1p(v) for v in values]
    centers = [min(points)+(max(points)-min(points))*i/2 for i in range(3)]
    for _ in range(50):
        groups = [[] for _ in centers]
        for value, point in zip(values, points):
            groups[min(range(3), key=lambda i: abs(point-centers[i]))].append(value)
        updated = [statistics.mean(math.log1p(v) for v in g) if g else centers[i]
                   for i, g in enumerate(groups)]
        if updated == centers:
            break
        centers = updated
    return [dict(count=len(g), **quantiles(g)) for g in groups if g]


def analyze(directory):
    files = sorted(set(directory.glob("*responses*.log")) | set(directory.glob("*chat-completions*.log")))
    rows, skipped = [], collections.Counter()
    for path in files:
        try:
            before = path.stat()
            log = path.read_text(errors="replace")
            after = path.stat()
            if (before.st_size, before.st_mtime_ns) != (after.st_size, after.st_mtime_ns):
                skipped["changed"] += 1
                continue
            info = log.split("=== HEADERS ===", 1)[0]
            url = re.search(r"^URL: (.+)$", info, re.M)
            if not url or url.group(1).split("?", 1)[0] not in ("/v1/responses", "/v1/chat/completions"):
                skipped["other_url"] += 1
                continue
            body = log.split("=== REQUEST BODY ===", 1)[1].lstrip()
            payload, _ = json.JSONDecoder().raw_decode(body)
            model = payload.get("model", "")
            if not model.startswith("gpt-"):
                skipped["other_model"] += 1
                continue
            timestamp = re.search(r"^Timestamp: (.+)$", info, re.M)
            reasoning = payload.get("reasoning") or {}
            effort = reasoning.get("effort", payload.get("reasoning_effort", "missing"))
            rows.append(dict(model=model, effort=effort, time=timestamp.group(1) if timestamp else "",
                             **metrics(payload)))
        except FileNotFoundError:
            skipped["disappeared"] += 1
        except (OSError, ValueError, IndexError, AttributeError, TypeError):
            skipped["unreadable_or_invalid"] += 1
    selected = [r for r in rows if re.fullmatch(r"gpt-\d+(?:\.\d+)*-(?:astra|sol)", r["model"])]
    eligible = [r for r in selected if r["effort"] in ("medium", "high", "xhigh", "max")]
    times = [r["time"] for r in rows if r["time"]]
    templates = collections.defaultdict(list)
    for row in selected:
        templates[row["template"]].append(row)
    return dict(scope=dict(files=len(files), parsed_gpt=len(rows), skipped=dict(skipped),
                           first=min(times, default=""), last=max(times, default="")),
                models=dict(collections.Counter(r["model"] for r in rows)),
                astra_sol_efforts=dict(collections.Counter(r["effort"] for r in selected)),
                medium_plus_count=len(eligible),
                first_user_chars=quantiles([r["first_user_chars"] for r in eligible]),
                context_chars=quantiles([r["context_chars"] for r in eligible]),
                context_log_clusters=clusters([r["context_chars"] for r in eligible]),
                repeated_templates=[dict(hash=key, count=len(g), first_user_chars=quantiles([r["first_user_chars"] for r in g]),
                                         context_chars=quantiles([r["context_chars"] for r in g]))
                                    for key, g in sorted(templates.items(), key=lambda item: -len(item[1]))[:12]])


def self_test():
    assert metrics({"instructions": "x"*10000, "tools": [{"description": "x"*10000}], "input": "hi"})["context_chars"] == 2
    value = metrics({"input": [{"role": "system", "content": "x"*10000},
                               {"role": "user", "content": "任务"},
                               {"type": "function_call_output", "output": "result"}]})
    assert value["first_user_chars"] == 2 and value["context_chars"] == 8
    assert len(clusters([2, 6, 100, 1000])) <= 3
    with tempfile.TemporaryDirectory() as directory:
        Path(directory, "v1-responses-test.log").write_text(
            '=== REQUEST INFO ===\nURL: /v1/responses\nTimestamp: 2026-11-20T00:00:00Z\n'
            '=== HEADERS ===\nAuthorization: Bearer do-not-leak\n=== REQUEST BODY ===\n'
            '{"model":"gpt-6.1-sol","input":"hi","reasoning":{"effort":"low"}}\n'
            '=== API REQUEST 1 ===\n'
            '{"model":"gpt-6-astra","input":"private upstream content","reasoning":{"effort":"high"}}')
        result = analyze(Path(directory))
        assert result["scope"]["parsed_gpt"] == 1 and result["medium_plus_count"] == 0
        assert result["models"] == {"gpt-6.1-sol": 1}
        assert "do-not-leak" not in json.dumps(result) and "private upstream" not in json.dumps(result)


if __name__ == "__main__":
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("directory", type=Path, nargs="?")
    parser.add_argument("--self-test", action="store_true")
    args = parser.parse_args()
    if args.self_test:
        self_test()
        print("self-test passed")
    elif args.directory is None or not args.directory.is_dir():
        parser.error("an existing local log directory is required")
    else:
        print(json.dumps(analyze(args.directory), ensure_ascii=False, indent=2))
