#!/usr/bin/env python3
# Copyright (c) 2026 TRV Enterprises LLC
# SPDX-License-Identifier: Apache-2.0
# See LICENSE file for details.
"""
Reconcile security-scanner output against the accepted-vulnerability registry
(security/accepted-vulns.yaml).

Reads scanner findings, splits them into ACTIONABLE vs KNOWN-ACCEPTED (an
exception that exists in the registry AND has not expired), and prints a report.

Usage:
  govulncheck -json ./...  | reconcile-scan.py --scanner govulncheck
  reconcile-scan.py --scanner codeql --input results.sarif

Exit code:
  0  no actionable findings (all clear, or all findings are known-accepted)
  1  one or more ACTIONABLE findings (caller decides whether to block)
  2  registry/parse error, or an exception in the registry has EXPIRED
     (expired exceptions are treated as actionable + flagged loudly)

This script only REPORTS + classifies; the Makefile decides what blocks. Keeping
accepted findings out of the actionable list is the point: a scan report that is
permanently red trains everyone to ignore it, so the next real finding lands in
a list nobody reads.

Ported from the Outpost repo's security/reconcile-scan.py, with a CodeQL SARIF
parser added — ts-store's static-analysis noise is SARIF alerts rather than
dependency advisories.
"""

import argparse
import datetime
import json
import os
import sys

REGISTRY = os.path.join(os.path.dirname(os.path.abspath(__file__)), "accepted-vulns.yaml")

REQUIRED_FIELDS = ("id", "scanner", "module", "accepted_by", "accepted_on", "expires_on", "reason")


def load_registry(path):
    """Load the accepted-vulns registry. Returns ({id: entry}, [errors]).
    Uses PyYAML when available, else a purpose-built parser for this file's
    flat shape so the gate has no hard dependency on PyYAML."""
    if not os.path.exists(path):
        return {}, [f"registry not found: {path}"]
    try:
        import yaml  # noqa

        with open(path) as f:
            data = yaml.safe_load(f) or {}
        entries = data.get("exceptions", []) or []
    except ImportError:
        entries = _parse_registry_fallback(path)

    by_id = {}
    errors = []
    for e in entries:
        eid = (e.get("id") or "").strip()
        if not eid:
            errors.append("registry entry missing 'id'")
            continue
        missing = [k for k in REQUIRED_FIELDS if not (e.get(k) or "").strip()]
        if missing:
            errors.append(f"{eid}: missing required field(s): {', '.join(missing)}")
        by_id[eid] = e
    return by_id, errors


def _parse_registry_fallback(path):
    """Minimal YAML-list parser for the flat `exceptions:` list in this file.
    Handles `- id: X` items with `key: value` lines and `>` folded blocks."""
    entries = []
    cur = None
    in_exceptions = False
    folding_key = None
    with open(path) as f:
        for raw in f:
            line = raw.rstrip("\n")
            stripped = line.strip()
            if stripped.startswith("#") or not stripped:
                continue
            if stripped == "exceptions:":
                in_exceptions = True
                continue
            if not in_exceptions:
                continue
            indent = len(line) - len(line.lstrip())
            if stripped.startswith("- "):
                if cur:
                    entries.append(cur)
                cur = {}
                folding_key = None
                rest = stripped[2:]
                if ":" in rest:
                    k, _, v = rest.partition(":")
                    cur[k.strip()] = v.strip()
                continue
            if cur is None:
                continue
            if folding_key is not None and indent > cur.get("_fold_indent", 0):
                cur[folding_key] = (cur.get(folding_key, "") + " " + stripped).strip()
                continue
            folding_key = None
            if ":" in stripped:
                k, _, v = stripped.partition(":")
                k = k.strip()
                v = v.strip()
                if v in (">", "|"):
                    folding_key = k
                    cur[k] = ""
                    cur["_fold_indent"] = indent
                else:
                    cur[k] = v
    if cur:
        entries.append(cur)
    for e in entries:
        e.pop("_fold_indent", None)
    return entries


def today():
    # Pass an explicit date via SOURCE_DATE_EPOCH for reproducible/testable runs.
    sde = os.environ.get("SOURCE_DATE_EPOCH")
    if sde:
        return datetime.datetime.utcfromtimestamp(int(sde)).date()
    return datetime.date.today()


def is_expired(entry):
    exp = (entry.get("expires_on") or "").strip()
    if not exp:
        return False  # registry validation reports the missing field separately
    try:
        return today() > datetime.date.fromisoformat(exp)
    except ValueError:
        return False


def parse_govulncheck(stream):
    """Parse `govulncheck -json`, returning SYMBOL-REACHABLE findings only.

    govulncheck emits two relevant record types:
      - `osv`     : the advisory CATALOG for everything in the import graph.
                    NOT findings — most are for code we never call.
      - `finding` : an actual finding with a `trace`. It is symbol-reachable
                    (our code calls the vulnerable function) iff some trace
                    frame names a `function`. Findings without one are
                    'imported but not called' — a lower tier, not actionable.

    This mirrors govulncheck's own default text-mode "Symbol Results"."""
    text = stream.read()
    dec = json.JSONDecoder()
    i, n = 0, len(text)
    catalog = {}
    reachable = {}
    while i < n:
        while i < n and text[i] in " \t\r\n":
            i += 1
        if i >= n:
            break
        try:
            obj, end = dec.raw_decode(text, i)
        except json.JSONDecodeError:
            break
        i = end
        if not isinstance(obj, dict):
            continue
        if isinstance(obj.get("osv"), dict):
            osv = obj["osv"]
            vid = osv.get("id")
            if vid:
                mod = ""
                affected = osv.get("affected") or []
                if affected and isinstance(affected, list):
                    mod = affected[0].get("package", {}).get("name", "")
                catalog[vid] = mod
        elif isinstance(obj.get("finding"), dict):
            f = obj["finding"]
            vid = f.get("osv")
            if not vid:
                continue
            trace = f.get("trace") or []
            if any(fr.get("function") for fr in trace):
                mod = trace[0].get("module", "") if trace else ""
                reachable.setdefault(vid, {"id": vid, "module": mod or "", "scanner": "govulncheck"})
    for vid, f in reachable.items():
        if not f["module"]:
            f["module"] = catalog.get(vid, "")
    return list(reachable.values())


def parse_codeql_sarif(stream):
    """Parse a CodeQL SARIF file into findings.

    Each result becomes one finding per (rule, file) pair — a rule firing on
    twenty lines of one file is one decision to make, not twenty. The id is
    "<rule-id>@<path>" so the registry can accept a rule at a specific file
    while leaving it actionable elsewhere; a bare "<rule-id>" entry in the
    registry accepts the rule repo-wide."""
    data = json.load(stream)
    findings = {}
    for run in data.get("runs", []) or []:
        # Rule metadata (severity, description) lives in the tool driver.
        rules = {}
        driver = (run.get("tool") or {}).get("driver") or {}
        for r in driver.get("rules", []) or []:
            rid = r.get("id")
            if not rid:
                continue
            props = r.get("properties") or {}
            rules[rid] = {
                "severity": props.get("security-severity") or props.get("problem.severity") or "",
                "name": (r.get("shortDescription") or {}).get("text", ""),
            }
        for res in run.get("results", []) or []:
            rid = res.get("ruleId") or ""
            if not rid:
                continue
            locs = res.get("locations") or []
            path = ""
            line = ""
            if locs:
                phys = (locs[0].get("physicalLocation") or {})
                path = (phys.get("artifactLocation") or {}).get("uri", "")
                line = (phys.get("region") or {}).get("startLine", "")
            key = f"{rid}@{path}" if path else rid
            meta = rules.get(rid, {})
            entry = findings.setdefault(
                key,
                {
                    "id": key,
                    "rule": rid,
                    "module": path or "(no location)",
                    "scanner": "codeql",
                    "severity": _sarif_severity(meta.get("severity", "")),
                    "title": meta.get("name", ""),
                    "lines": [],
                },
            )
            if line:
                entry["lines"].append(str(line))
    return list(findings.values())


def _sarif_severity(raw):
    """CodeQL emits security-severity as a CVSS-ish number; map to a word so
    the report reads like the other scanners'."""
    try:
        v = float(raw)
    except (TypeError, ValueError):
        return str(raw or "")
    if v >= 9.0:
        return "critical"
    if v >= 7.0:
        return "high"
    if v >= 4.0:
        return "medium"
    return "low"


def match_registry(finding, registry):
    """Return the registry entry covering this finding, or None.
    A CodeQL finding matches either its exact "<rule>@<path>" id or a
    repo-wide bare "<rule>" entry."""
    entry = registry.get(finding["id"])
    if entry:
        return entry
    rule = finding.get("rule")
    if rule:
        return registry.get(rule)
    return None


def main():
    ap = argparse.ArgumentParser(description=__doc__, formatter_class=argparse.RawDescriptionHelpFormatter)
    ap.add_argument("--scanner", required=True, choices=["govulncheck", "codeql"])
    ap.add_argument("--input", help="read scanner output from a file instead of stdin")
    ap.add_argument("--registry", default=REGISTRY)
    ap.add_argument("--label", help="override the report header")
    args = ap.parse_args()

    registry, reg_errors = load_registry(args.registry)
    for err in reg_errors:
        print(f"⚠ registry: {err}", file=sys.stderr)

    stream = open(args.input) if args.input else sys.stdin
    try:
        if args.scanner == "govulncheck":
            findings = parse_govulncheck(stream)
        else:
            findings = parse_codeql_sarif(stream)
    except (json.JSONDecodeError, ValueError) as e:
        print(f"✗ could not parse {args.scanner} output: {e}", file=sys.stderr)
        return 2
    finally:
        if args.input:
            stream.close()

    actionable, accepted, expired = [], [], []
    for f in findings:
        entry = match_registry(f, registry)
        if entry and is_expired(entry):
            expired.append((f, entry))
        elif entry:
            accepted.append((f, entry))
        else:
            actionable.append(f)

    label = args.label or (
        "govulncheck (Go deps + stdlib)" if args.scanner == "govulncheck" else "CodeQL (static analysis)"
    )
    print(f"── {label} — reconciled against accepted-vulns registry ──")

    if accepted:
        print(f"\n  Known-accepted ({len(accepted)}) — risk consciously accepted, see security/accepted-vulns.yaml:")
        for f, e in accepted:
            print(f"    · {f['id']}  (until {e.get('expires_on', '?')})")

    if expired:
        print(f"\n  ⚠ EXPIRED EXCEPTIONS ({len(expired)}) — re-review or remediate, now ACTIONABLE:")
        for f, e in expired:
            print(f"    ! {f['id']}  (expired {e.get('expires_on', '?')})")

    if actionable:
        print(f"\n  ACTIONABLE ({len(actionable)}) — not in the accepted registry:")
        for f in actionable:
            extra = f"  [{f['severity']}]" if f.get("severity") else ""
            lines = f"  lines {','.join(f['lines'])}" if f.get("lines") else ""
            print(f"    ✗ {f['id']}{extra}{lines}")
    else:
        print("\n  ACTIONABLE (0) — clean ✓")

    print()
    if reg_errors or expired:
        return 2
    return 1 if actionable else 0


if __name__ == "__main__":
    sys.exit(main())
