# Releasing ts-store

This document codifies how releases are cut and how their GitHub release notes are written. The mechanical steps (version bump, build, tag, push, binary upload) are automated by `make release` and the `release-binaries.yml` workflow. The **release notes themselves are written by hand** — Claude drafts them, the maintainer reviews.

## The cadence

```bash
make release VERSION=vX.Y.Z
```

This Makefile target:

1. Updates the version in `internal/version/version.go`.
2. Builds the server + collector binaries for `linux/amd64` and `linux/arm64`.
3. Stages and commits the version bump.
4. Creates an annotated tag `vX.Y.Z`.
5. Pushes `main` and the tag to origin.

On tag push, two workflows fire:

- `docker-publish.yml` — builds and pushes the multi-arch image to `ghcr.io/trv-enterprises/ts-store:vX.Y.Z`.
- `release-binaries.yml` — rebuilds the binaries in CI and attaches them to a GitHub Release. If the release does not already exist, `softprops/action-gh-release` **creates one automatically** with a default body that is just the commit list and a compare link.

That auto-generated body is a placeholder. It must be replaced with hand-written notes.

## The release-notes contract

Release notes are not changelogs. They are written for the operator running `docker pull` or `tsstore` on a Pi. They answer: *do I need to do anything to upgrade, and what do I get if I do?*

### Structure

Use these sections, in this order. Omit any section that doesn't apply.

```markdown
## Highlights

- **`<endpoint or feature name>`** — one-line "why you care" framing. Lead with what the user sees, not the internal implementation.
- Repeat for each user-visible change.

## Breaking changes

Lead with a one-sentence statement of what breaks and what action the operator must take. Then per-change subsections explaining the old shape → new shape.

## Compatibility

One paragraph: is this a safe drop-in? Are there config migrations? Does the on-disk format change? If everything is additive, say so explicitly — operators read this section first.
```

For docs-only or no-op releases, say so plainly in **Highlights** ("Documentation-only release. No binary or runtime behavior changes …") and use **Compatibility** to confirm it's a safe drop-in.

### Title format

```
vX.Y.Z — <short tagline>
```

The tagline should be a noun phrase describing the headline change (e.g. "Activity counters + public /stats", "Webhook/WS/MQTT alerts", "Docs alignment for v0.6.8"). Skip the tagline only for trivial patch releases that genuinely have nothing to highlight.

### Tone

- Plain, direct, present tense. "Adds X." not "This release adds X."
- Use code formatting for endpoint paths, flag names, config keys, and field names.
- Link to the compare URL at the bottom: `**Full Changelog**: https://github.com/trv-enterprises/ts-store/compare/vA.B.C...vX.Y.Z`.
- No marketing language, no emoji decorations in body copy (the auto-generated trailer "🤖 Generated with [Claude Code]…" is fine to keep on Claude-drafted notes; not required).

## When Claude writes the notes

Claude drafts release notes for every tagged release. The flow:

1. After `make release VERSION=vX.Y.Z` finishes and the tag is pushed, ask Claude for release notes:
   > "Draft release notes for vX.Y.Z."
2. Claude reads `git log <previous-tag>..vX.Y.Z`, inspects the actual diffs for user-visible changes, drafts notes in the structure above, and shows them for review.
3. On approval, Claude posts them with:
   ```bash
   GH_TOKEN=$TRVE_GH_TOKEN gh release edit vX.Y.Z --repo trv-enterprises/ts-store \
     --title "vX.Y.Z — <tagline>" \
     --notes "$(cat <<'EOF'
   …notes body…
   EOF
   )"
   ```
   (Use `gh release create` instead if the auto-generated release doesn't exist yet — this can happen if `release-binaries.yml` is still running.)

If the maintainer tags a release without prompting Claude, Claude should notice the bare auto-generated body and offer to rewrite it.

## Backfilling missed releases

If a prior release shipped with only the auto-generated commit-list body, treat it the same way: read `git log <prev>..<tag>`, draft proper notes, post with `gh release edit`. Don't rewrite history (tags stay put); just fix the release-page body.

## Token note

Use `$TRVE_GH_TOKEN` (not the default `gh` auth) for any `gh` command against `trv-enterprises/*` — it's the org-scoped token. Never echo the token value.
