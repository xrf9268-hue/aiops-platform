#!/usr/bin/env python3
"""Single source of truth for GitHub Codex review-completion predicates (#870).

Codex is the `chatgpt-codex-connector[bot]` GitHub App. Detect it by its stable
numeric account id, never by the `[bot]`-suffixed login string: the login is
easy to drop and differs across endpoints, so a bare-login filter silently
matches nothing and reports false "no review yet". See
docs/runbooks/pr-review-merge-protocol.md section 4 and
docs/design/codex-review-detection.md (#870).

Empirical identity + signaling contract (re-validate if Codex changes
signaling — design D-Q4):

  * Codex account: id=199175422, login="chatgpt-codex-connector[bot]", type=Bot
    (identical on reviews, issue comments, and reactions).
  * The only reliable, current-head-bound structured signal is a *findings*
    review object whose commit_id is the reviewed head. A clean review leaves no
    such object (only a PR-body +1 / natural-language comment), so "clean" is
    NOT structurally detectable for unattended automation; its absence is
    NOT-CONFIRMED (clean-or-not-reviewed), handed to a human, never auto-merged.
"""

import json
import sys

# Authoritative identity. The id is stable and unambiguous; login/type are
# secondary cross-checks (login can drift across an App reinstall).
CODEX_BOT_ID = 199175422
CODEX_BOT_LOGIN = "chatgpt-codex-connector[bot]"


def classify_identity(user):
    """Classify a GitHub simple-user against Codex's identity contract.

    Returns ``(verdict, note)`` where verdict is one of:

      * ``"match"``  - authoritative Codex (id matches, type Bot). A drifted
                       login is tolerated with a note (likely App reinstall).
      * ``"reject"`` - looks like Codex but fails the contract (type not Bot, or
                       the login matches over a wrong/absent id: possible spoof).
                       Callers MUST fail closed.
      * ``"skip"``   - an unrelated author; ignore this object.

    The numeric id is authoritative; never match a bare login without ``[bot]``.
    """
    user = user or {}
    uid = user.get("id")
    login = user.get("login") or ""
    utype = user.get("type") or ""
    if uid == CODEX_BOT_ID:
        if utype != "Bot":
            return ("reject", "id %d matches but type is %r, not Bot" % (CODEX_BOT_ID, utype))
        if login != CODEX_BOT_LOGIN:
            return ("match", "login %r drifted from %r (id authoritative)" % (login, CODEX_BOT_LOGIN))
        return ("match", "")
    if login == CODEX_BOT_LOGIN:
        return ("reject", "login %r without id %d (possible spoof)" % (login, CODEX_BOT_ID))
    return ("skip", "non-Codex author")


def find_findings_review(reviews, head_sha, trigger_time):
    """Detect a Codex *findings* review object bound to the reviewed head.

    A findings review is an authoritative-Codex review whose ``commit_id`` is
    ``head_sha`` and whose ``submitted_at`` is not older than ``trigger_time``
    (ISO-8601 timestamps compare lexically, like git's own UTC ``Z`` form).

    Returns ``(status, payload)``:

      * ``("FINDINGS", {...})`` - a matching review object (completion +
        attribution: Codex reviewed *this* head).
      * ``("NONE", None)``      - no such object yet (keep polling / time out).
      * ``("REJECT", reason)``  - an identity conflict anywhere in the set;
        callers MUST fail closed.

    A spoof anywhere in the set fails the whole classification closed, even when
    a legitimate findings object also appears.
    """
    found = None
    for review in reviews:
        verdict, why = classify_identity(review.get("user"))
        if verdict == "reject":
            return ("REJECT", why)
        if verdict != "match" or found is not None:
            continue
        if review.get("commit_id") != head_sha:
            continue
        submitted = review.get("submitted_at") or ""
        if trigger_time and submitted < trigger_time:
            continue
        found = {
            "review_id": review.get("id"),
            "commit_id": review.get("commit_id"),
            "submitted_at": submitted,
        }
    if found is not None:
        return ("FINDINGS", found)
    return ("NONE", None)


def _flatten(raw):
    """Normalise a ``gh api --paginate --slurp`` payload to a flat list.

    ``--slurp`` yields a list of per-page lists; a single page is a plain list.
    """
    if isinstance(raw, list) and raw and isinstance(raw[0], list):
        return [item for page in raw for item in page]
    if isinstance(raw, list):
        return raw
    return []


def _cmd_find_findings(argv):
    head_sha = argv[0] if len(argv) > 0 else ""
    trigger_time = argv[1] if len(argv) > 1 else ""
    reviews = _flatten(json.load(sys.stdin))
    status, payload = find_findings_review(reviews, head_sha, trigger_time)
    if status == "REJECT":
        sys.stderr.write("codex identity conflict: %s\n" % payload)
        return 3
    if status == "FINDINGS":
        sys.stdout.write("FINDINGS %s\n" % payload.get("review_id"))
        return 0
    sys.stdout.write("NONE\n")
    return 0


def _cmd_classify_identity():
    verdict, note = classify_identity(json.load(sys.stdin))
    sys.stdout.write("%s %s\n" % (verdict, note))
    return 0


def main(argv):
    if not argv:
        sys.stderr.write(
            "usage: codex_review_signal.py {find-findings <head_sha> <trigger_time>|classify-identity}\n"
        )
        return 2
    cmd, rest = argv[0], argv[1:]
    if cmd == "find-findings":
        return _cmd_find_findings(rest)
    if cmd == "classify-identity":
        return _cmd_classify_identity()
    sys.stderr.write("unknown command: %s\n" % cmd)
    return 2


if __name__ == "__main__":
    sys.exit(main(sys.argv[1:]))
