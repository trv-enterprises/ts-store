# Security scanning and accepted risk

Two scanners run against this repo, and one registry decides what counts as noise.

| Scanner | Runs | Finds |
|---|---|---|
| `govulncheck` | PR checks (`Vulnerability scan`) + `make security-scan` | Known CVEs in Go dependencies and the stdlib, filtered to **symbol-reachable** findings |
| CodeQL | PR checks (`CodeQL` / `Analyze (Go)`) | Static-analysis findings — taint flow, injection, SSRF |

## The registry

[`accepted-vulns.yaml`](accepted-vulns.yaml) is the single source of truth for findings we have **consciously accepted**: the blast radius does not reach this system, or remediation is gated upstream. Every entry carries who accepted it, why, and — mandatory — **when the acceptance expires**. Nothing is accepted permanently; an expired entry becomes actionable again and fails the scan loudly.

Adding an entry is a risk-acceptance decision, code-reviewed like any other change. Write the justification as if defending it in an audit.

**Secret leaks are never accepted here.** A committed secret is always actionable: rotate it, then purge it from history.

## Running it

```bash
make security-scan                              # govulncheck, reconciled
make security-scan-codeql SARIF=results.sarif   # CodeQL SARIF, reconciled
```

CodeQL runs in CI rather than locally; to reconcile a real run:

```bash
ID=$(gh api repos/trv-enterprises/ts-store/code-scanning/analyses --jq '.[0].id')
gh api "repos/trv-enterprises/ts-store/code-scanning/analyses/$ID" \
  -H "Accept: application/sarif+json" > results.sarif
make security-scan-codeql SARIF=results.sarif
```

Exit codes from `reconcile-scan.py`: `0` clean or all-accepted, `1` actionable findings, `2` registry error or an **expired** exception.

## Why a registry instead of just dismissing alerts

A check that is permanently red trains everyone to ignore it, so the next real finding lands in a list nobody reads. The registry converts "known red" into "known-accepted, with an expiry date and a reason", leaving the actionable list meaningful. It also keeps the reasoning in the repo and under review, rather than in a dismissal dropdown in the GitHub UI.

### Dismissing an accepted CodeQL alert

An accepted finding still shows a red `CodeQL` check on any PR that touches its
file, which is corrosive — a check that always fails is a check nobody reads.
The registry is the decision; **dismissal is how that decision reaches the
check**. Do it per alert, never by widening `query-filters`:

```bash
# Find the alert on the PR's merge ref
gh api "repos/trv-enterprises/ts-store/code-scanning/alerts?ref=refs/pull/<N>/merge&state=open" \
  --jq '.[] | "\(.number) \(.rule.id) \(.most_recent_instance.location.path):\(.most_recent_instance.location.start_line)"'

# Dismiss it, citing the registry entry (comment max 280 chars)
gh api -X PATCH repos/trv-enterprises/ts-store/code-scanning/alerts/<ID> \
  -f state=dismissed -f dismissed_reason="won't fix" \
  -f dismissed_comment="Accepted: security/accepted-vulns.yaml, <rule-id> (expires <date>). <why it is not reachable>."
```

Before dismissing, **confirm the finding is genuinely the accepted class** —
reconcile the PR's real SARIF, don't assume from the rule name:

```bash
ID=$(gh api "repos/trv-enterprises/ts-store/code-scanning/analyses?ref=refs/pull/<N>/merge" --jq '.[0].id')
gh api "repos/trv-enterprises/ts-store/code-scanning/analyses/$ID" \
  -H "Accept: application/sarif+json" > /tmp/pr.sarif
make security-scan-codeql SARIF=/tmp/pr.sarif   # must report ACTIONABLE (0)
```

If it reports anything actionable, that is a new finding: triage it, do not
dismiss it. A dismissal whose comment does not point at a registry entry is a
process failure — the entry carries the reasoning, the reviewer, and the expiry
date; the dismissal is only the switch.

Prefer this to a `query-filters` exclusion whenever the rule is one we want to
keep hearing about elsewhere. `go/path-injection` is the standing example: it is
accepted at known-guarded sites but left enabled repo-wide, because this store
builds paths from user-supplied names and a genuinely new traversal bug must
still surface.

CodeQL findings may additionally get an **in-code suppression** so the check
itself goes green. The registry entry is the justification; the suppression is what stops it re-reporting. Every suppression should name its registry entry, and every `codeql` registry entry records where its suppression lives (`suppression:` field) — so the two can't quietly drift apart.

## Fix by default; accept as the exception

Accepting is the fallback, not the first move. The log-injection findings in `internal/alerts/worker.go`, for instance, were **fixed** (`logSafe` scrubs control characters and bounds length before user-controlled values reach a log line) rather than accepted — journald output is itself ingested by the journal-logs collector, so forged log lines are a real problem here, and the fix was cheap.

Accept when the finding is genuinely inapplicable — a guarded path CodeQL can't see, or behavior that *is* the product's design (operator-configured webhook sinks) — and say precisely why.
