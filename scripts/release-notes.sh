#!/usr/bin/env bash
# Emits the body for a GitHub release.
#
# Authors write the notes between <!-- release-notes --> markers in the PR description.
# That description reaches the merge commit (GitHub pre-fills the squash message from it),
# but it arrives hard-wrapped at 72 columns, which mangles anything whose meaning depends
# on line structure — a table row is the clearest casualty. So the PR body is read directly
# when the commit came from one, and the commit itself is the fallback: a direct push to
# main has no PR, and the API may be unavailable.
#
# With no markers (or an empty block) the commit subject is the note. That is deliberate:
# a release must never fail for want of a description.
set -euo pipefail

REF="${1:-HEAD}"

MESSAGE="$(git log -1 --format=%B "$REF")"

# Markers are only recognised on a line of their own, and only the first block is read.
# Prose that mentions the markers inline (documentation about this very mechanism) must
# not reopen the capture or extend it past the close.
extract_block() {
    awk '
        done_capturing { next }
        /^[[:space:]]*<!--[[:space:]]*release-notes[[:space:]]*-->[[:space:]]*$/ {
            if (!seen_open) { seen_open = 1; capture = 1 }
            next
        }
        /^[[:space:]]*<!--[[:space:]]*\/release-notes[[:space:]]*-->[[:space:]]*$/ {
            if (capture) { capture = 0; done_capturing = 1 }
            next
        }
        capture { print }
    '
}

# Resolve the PR this commit was merged from, if any. Used for both the notes body and the
# closed-issue enrichment below, so it is looked up once.
PR_NUMBER=""
if command -v gh >/dev/null 2>&1 && gh auth status >/dev/null 2>&1; then
    PR_NUMBER="$(gh api "repos/${GITHUB_REPOSITORY:-}/commits/$(git rev-parse "$REF")/pulls" \
        --jq '.[].number' 2>/dev/null | head -1 || true)"
fi

NOTES=""
if [[ -n "$PR_NUMBER" ]]; then
    # The PR body is the authoritative copy: unwrapped, exactly as the author wrote it.
    NOTES="$(gh pr view "$PR_NUMBER" --repo "${GITHUB_REPOSITORY:-}" --json body --jq .body 2>/dev/null \
        | extract_block || true)"
fi

if [[ -z "${NOTES//[[:space:]]/}" ]]; then
    NOTES="$(printf '%s' "$MESSAGE" | extract_block)"
fi

# Trim leading and trailing blank lines, leaving interior blank lines intact.
NOTES="$(printf '%s\n' "$NOTES" | awk '
    { lines[NR] = $0 }
    END {
        first = 1; last = NR
        while (first <= NR && lines[first] ~ /^[[:space:]]*$/) first++
        while (last >= first && lines[last] ~ /^[[:space:]]*$/) last--
        for (i = first; i <= last; i++) print lines[i]
    }
')"

# The subject is the PR title (squash_merge_commit_title=PR_TITLE), minus the "(#123)"
# suffix GitHub appends and any "+semver:" directive meant for GitVersion, not readers.
TITLE="$(git log -1 --format=%s "$REF" \
    | sed -E 's/[[:space:]]*\(#[0-9]+\)[[:space:]]*$//' \
    | sed -E 's/[[:space:]]*\+semver:[[:space:]]*[a-z]+[[:space:]]*//g' \
    | sed -E 's/[[:space:]]+$//')"

if [[ -z "${NOTES//[[:space:]]/}" ]]; then
    # No block: the title carries the whole note.
    printf '%s\n' "$TITLE"
    exit 0
fi

# Byline: release date, the PR the commit came from, and any issues it closes — all read
# off the commit, so nothing has to be restated in the block. Each part is omitted when
# absent, and the whole line disappears for a commit with none of them (a direct push
# with no issue references).
DATE="$(git log -1 --format=%cs "$REF")"
SUBJECT="$(git log -1 --format=%s "$REF")"
BODY="$(git log -1 --format=%B "$REF")"

BYLINE="$DATE"

PR="$(printf '%s' "$SUBJECT" | grep -oE '\(#[0-9]+\)[[:space:]]*$' | grep -oE '#[0-9]+' 2>/dev/null || true)"
if [[ -n "$PR" ]]; then
    BYLINE="$BYLINE · $PR"
fi

# Closed issues come from two places that can each hold what the other does not:
#
#   - "Closes #12" written in the PR body, which reaches the commit with everything else
#   - the PR's linked-issue metadata, which the sidebar can set with no matching text
#
# So the two are unioned rather than one falling back to the other. The API half is
# best-effort: no token, no network or no PR simply leaves the text-derived list as-is,
# which is what a direct push to main gets anyway.
#
# `|| true` throughout: grep exits 1 when nothing matches, which under `set -o pipefail`
# would otherwise abort the release.
ISSUES="$(printf '%s' "$BODY" \
    | grep -oiE '(closes|fixes|resolves)[[:space:]]+#[0-9]+' \
    | grep -oE '[0-9]+' || true)"

if [[ -n "$PR_NUMBER" ]]; then
    LINKED="$PR_NUMBER"
    if [[ -n "$LINKED" ]]; then
        # --repo explicitly: gh would otherwise infer it from the local remote, which is
        # not necessarily the repo being released.
        META="$(gh pr view "$LINKED" --repo "${GITHUB_REPOSITORY:-}" \
            --json closingIssuesReferences \
            --jq '.closingIssuesReferences[].number' 2>/dev/null || true)"
        ISSUES="$(printf '%s\n%s' "$ISSUES" "$META")"
    fi
fi

# Deduplicate, drop blanks, sort numerically so the list reads in issue order.
CLOSES="$(printf '%s\n' "$ISSUES" \
    | grep -oE '[0-9]+' \
    | sort -n -u \
    | sed 's/^/#/' \
    | paste -sd ',' - \
    | sed 's/,/, /g' || true)"
if [[ -n "$CLOSES" ]]; then
    BYLINE="$BYLINE · closes $CLOSES"
fi

# Give the body a heading, matching how the release list reads: the release itself is
# named for the version, so the body leads with what the release is. An author who opens
# the block with their own heading keeps it, and supplies their own byline if they want one.
if [[ "$NOTES" == '#'* ]]; then
    printf '%s\n' "$NOTES"
else
    printf '## %s\n\n*%s*\n\n%s\n' "$TITLE" "$BYLINE" "$NOTES"
fi
