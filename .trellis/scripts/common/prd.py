"""PRD readiness helpers for Trellis tasks."""

from __future__ import annotations

from pathlib import Path


_PLACEHOLDER_LINES = {
    "TBD",
    "TBD.",
    "- TBD",
    "- TBD.",
    "- [ ] TBD",
    "- [ ] TBD.",
}

_REQUIRED_SECTIONS = ("requirements", "acceptance criteria")


def _section_name(line: str) -> str | None:
    stripped = line.strip()
    if not stripped.startswith("## "):
        return None
    return stripped[3:].strip().lower()


def _section_kind(name: str) -> str | None:
    for kind in _REQUIRED_SECTIONS:
        if name.startswith(kind):
            return kind
    return None


def _collect_sections(lines: list[str]) -> list[tuple[str, list[str]]]:
    sections: list[tuple[str, list[str]]] = []
    current: str | None = None

    for line in lines:
        stripped = line.strip()
        section = _section_name(stripped)
        if section is not None:
            current = section
            sections.append((current, []))
            continue
        if stripped.startswith("# "):
            current = None
            continue
        if current is not None:
            sections[-1][1].append(stripped)

    return sections


def _has_placeholder(lines: list[str]) -> bool:
    return any(line in _PLACEHOLDER_LINES for line in lines)


def _has_real_content(lines: list[str]) -> bool:
    return any(line and line not in _PLACEHOLDER_LINES for line in lines)


def _sections_for_kind(
    sections: list[tuple[str, list[str]]], kind: str
) -> list[list[str]]:
    return [lines for name, lines in sections if _section_kind(name) == kind]


def _required_sections_have_content(sections: list[tuple[str, list[str]]]) -> bool:
    for kind in _REQUIRED_SECTIONS:
        matching = _sections_for_kind(sections, kind)
        if not matching:
            return False
        if any(_has_placeholder(lines) for lines in matching):
            return False
        if not any(_has_real_content(lines) for lines in matching):
            return False
    return True


def _legacy_sections_have_content(sections: list[tuple[str, list[str]]]) -> bool:
    acceptance_sections = _sections_for_kind(sections, "acceptance criteria")
    if not acceptance_sections:
        return False
    if any(_has_placeholder(lines) for lines in acceptance_sections):
        return False
    if not any(_has_real_content(lines) for lines in acceptance_sections):
        return False

    detail_sections = [
        lines for name, lines in sections if _section_kind(name) is None
    ]
    return any(
        not _has_placeholder(section_lines) and _has_real_content(section_lines)
        for section_lines in detail_sections
    )


def prd_is_ready(prd_path: Path) -> bool:
    """Return True when a PRD has non-placeholder planning content."""
    if not prd_path.is_file():
        return False
    try:
        lines = prd_path.read_text(encoding="utf-8").splitlines()
    except (OSError, UnicodeDecodeError):
        return False
    if not any(line.strip() for line in lines):
        return False
    sections = _collect_sections(lines)
    if _sections_for_kind(sections, "requirements"):
        return _required_sections_have_content(sections)
    return _legacy_sections_have_content(sections)
