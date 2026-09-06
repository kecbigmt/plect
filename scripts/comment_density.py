#!/usr/bin/env python3
"""Enforce the comment-shape rules CLAUDE.md's Comments section states,
scoped to one PR's added/modified lines (base-ref..head-ref), for
.go/.ts/.tsx/.sql/.toml files.

Three checks, each reported as "<file>:<line>: <check> <value> > <limit>":
  density        comment lines / added non-blank lines, for files with at
                 least DENSITY_FLOOR_LINES added non-blank lines (below the
                 floor the ratio is not meaningful and the file is skipped).
  block-length   longest run of consecutive added comment-only lines.
  duplication    a normalized comment sentence appearing more than
                 DUPLICATION_LIMIT time(s) across the PR's added lines
                 (every site is reported).

Deliberately structural, not content-based: no prose literal or specific
word is ever matched. The one content-shaped exception (Go build-constraint
lines) is a language-syntax carve-out documented in CLAUDE.md, not a prose
match, and a license-header block is recognized only by its position (line 1
of a newly added file), never by its wording.

Full head-file content is decoded (via `git show <head>:<path>`) rather than
reconstructed from the diff body: a multi-line block comment or a diff hunk
using -U0 does not, by itself, say whether a given added line sits inside a
comment that started before the hunk. Reading the whole file lets language
lexing track that state correctly; the diff's hunk headers are still what
decide which of those lines counts as "added" for scoring.

Usage: comment_density.py <repo-root> <base-ref> <head-ref>
"""
import re
import subprocess
import sys

DENSITY_FLOOR_LINES = 30
DENSITY_LIMIT_PERCENT = 15
BLOCK_LENGTH_LIMIT = 8
DUPLICATION_LIMIT = 1
DUPLICATION_MIN_WORDS = 6

ALLOWED_EXTS = {".go", ".ts", ".tsx", ".sql", ".toml"}
GO_TS_EXTS = {".go", ".ts", ".tsx"}
SQL_EXTS = {".sql"}
TOML_EXTS = {".toml"}

GO_BUILD_LINE_RE = re.compile(r"^//go:build\b|^//\s*\+build\b")


def run_git(root, args):
    result = subprocess.run(
        ["git", "-C", root] + args,
        stdout=subprocess.PIPE,
        stderr=subprocess.PIPE,
    )
    return result


def changed_files(root, base, head):
    result = run_git(
        root,
        [
            "diff",
            "--no-renames",
            "--diff-filter=AM",
            "--name-only",
            base,
            head,
            "--",
            "*.go",
            "*.ts",
            "*.tsx",
            "*.sql",
            "*.toml",
        ],
    )
    if result.returncode != 0:
        sys.stderr.write(result.stderr.decode("utf-8", "replace"))
        sys.exit(2)
    paths = [p for p in result.stdout.decode("utf-8").splitlines() if p]
    return [p for p in paths if not excluded(root, p)]


def excluded(root, path):
    if "/testdata/" in path or path.startswith("testdata/"):
        return True
    if re.search(r"(^|/)sqlcgen/", path):
        return True
    if path.startswith("web/api/generated/") or "/web/api/generated/" in path:
        return True
    result = run_git(root, ["check-attr", "linguist-generated", "--", path])
    if result.returncode == 0:
        out = result.stdout.decode("utf-8", "replace")
        if out.rstrip("\n").endswith(("set", "true")):
            return True
    return False


def is_new_file(root, base, path):
    result = run_git(root, ["cat-file", "-e", f"{base}:{path}"])
    return result.returncode != 0


HUNK_HEADER_RE = re.compile(r"^@@ -\d+(?:,\d+)? \+(\d+)(?:,(\d+))? @@")


def added_line_numbers(root, base, head, path):
    result = run_git(root, ["diff", "--no-renames", "-U0", base, head, "--", path])
    if result.returncode != 0:
        sys.stderr.write(result.stderr.decode("utf-8", "replace"))
        sys.exit(2)
    lines = set()
    for line in result.stdout.decode("utf-8", "replace").splitlines():
        m = HUNK_HEADER_RE.match(line)
        if not m:
            continue
        start = int(m.group(1))
        count = int(m.group(2)) if m.group(2) is not None else 1
        for ln in range(start, start + count):
            lines.add(ln)
    return lines


def file_lines(root, head, path):
    result = run_git(root, ["show", f"{head}:{path}"])
    if result.returncode != 0:
        sys.stderr.write(result.stderr.decode("utf-8", "replace"))
        sys.exit(2)
    return result.stdout.decode("utf-8", "replace").splitlines()


def classify_go_ts(lines):
    NORMAL, BLOCK_COMMENT, DQUOTE, SQUOTE, BACKTICK = range(5)
    state = NORMAL
    out = []
    for line in lines:
        i, n = 0, len(line)
        code_chars = 0
        parts = []
        while i < n:
            c = line[i]
            if state == BLOCK_COMMENT:
                j = line.find("*/", i)
                if j == -1:
                    parts.append(line[i:])
                    i = n
                else:
                    parts.append(line[i : j + 2])
                    i = j + 2
                    state = NORMAL
                continue
            if state in (DQUOTE, SQUOTE, BACKTICK):
                closer = '"' if state == DQUOTE else ("'" if state == SQUOTE else "`")
                if c == "\\":
                    code_chars += 1
                    i += 2
                    continue
                code_chars += 1
                if c == closer:
                    state = NORMAL
                i += 1
                continue
            # NORMAL
            if c == "/" and i + 1 < n and line[i + 1] == "/":
                parts.append(line[i:])
                i = n
                continue
            if c == "/" and i + 1 < n and line[i + 1] == "*":
                j = line.find("*/", i + 2)
                if j == -1:
                    state = BLOCK_COMMENT
                    parts.append(line[i:])
                    i = n
                else:
                    parts.append(line[i : j + 2])
                    i = j + 2
                continue
            if c == '"':
                state = DQUOTE
                code_chars += 1
                i += 1
                continue
            if c == "'":
                state = SQUOTE
                code_chars += 1
                i += 1
                continue
            if c == "`":
                state = BACKTICK
                code_chars += 1
                i += 1
                continue
            if not c.isspace():
                code_chars += 1
            i += 1
        out.append((code_chars, "".join(parts).strip()))
    return out


def classify_sql(lines):
    NORMAL, BLOCK_COMMENT, SQUOTE, DQUOTE = range(4)
    state = NORMAL
    out = []
    for line in lines:
        i, n = 0, len(line)
        code_chars = 0
        parts = []
        while i < n:
            c = line[i]
            if state == BLOCK_COMMENT:
                j = line.find("*/", i)
                if j == -1:
                    parts.append(line[i:])
                    i = n
                else:
                    parts.append(line[i : j + 2])
                    i = j + 2
                    state = NORMAL
                continue
            if state in (SQUOTE, DQUOTE):
                closer = "'" if state == SQUOTE else '"'
                code_chars += 1
                if c == closer:
                    state = NORMAL
                i += 1
                continue
            # NORMAL: SQLite string literals have no backslash escape, only
            # a doubled closing-quote character, which a plain toggle
            # already handles (close then immediately reopen).
            if c == "-" and i + 1 < n and line[i + 1] == "-":
                parts.append(line[i:])
                i = n
                continue
            if c == "/" and i + 1 < n and line[i + 1] == "*":
                j = line.find("*/", i + 2)
                if j == -1:
                    state = BLOCK_COMMENT
                    parts.append(line[i:])
                    i = n
                else:
                    parts.append(line[i : j + 2])
                    i = j + 2
                continue
            if c == "'":
                state = SQUOTE
                code_chars += 1
                i += 1
                continue
            if c == '"':
                state = DQUOTE
                code_chars += 1
                i += 1
                continue
            if not c.isspace():
                code_chars += 1
            i += 1
        out.append((code_chars, "".join(parts).strip()))
    return out


def classify_toml(lines):
    NORMAL, BASIC, LITERAL, MULTI_BASIC, MULTI_LITERAL = range(5)
    state = NORMAL
    out = []
    for line in lines:
        i, n = 0, len(line)
        code_chars = 0
        parts = []
        while i < n:
            c = line[i]
            if state == MULTI_BASIC or state == MULTI_LITERAL:
                triple = '"""' if state == MULTI_BASIC else "'''"
                j = line.find(triple, i)
                if j == -1:
                    code_chars += len(line[i:].strip())
                    i = n
                else:
                    code_chars += len(line[i:j].strip())
                    i = j + 3
                    state = NORMAL
                continue
            if state == BASIC:
                if c == "\\":
                    code_chars += 1
                    i += 2
                    continue
                code_chars += 1
                if c == '"':
                    state = NORMAL
                i += 1
                continue
            if state == LITERAL:
                code_chars += 1
                if c == "'":
                    state = NORMAL
                i += 1
                continue
            # NORMAL
            if c == "#":
                parts.append(line[i:])
                i = n
                continue
            if line[i : i + 3] == '"""':
                state = MULTI_BASIC
                code_chars += 3
                i += 3
                continue
            if line[i : i + 3] == "'''":
                state = MULTI_LITERAL
                code_chars += 3
                i += 3
                continue
            if c == '"':
                state = BASIC
                code_chars += 1
                i += 1
                continue
            if c == "'":
                state = LITERAL
                code_chars += 1
                i += 1
                continue
            if not c.isspace():
                code_chars += 1
            i += 1
        out.append((code_chars, "".join(parts).strip()))
    return out


def classify_file(lines, ext):
    if ext in GO_TS_EXTS:
        raw = classify_go_ts(lines)
    elif ext in SQL_EXTS:
        raw = classify_sql(lines)
    elif ext in TOML_EXTS:
        raw = classify_toml(lines)
    else:
        raise ValueError(f"unsupported extension: {ext}")
    kinds = {}
    comment_text = {}
    for idx, (code_chars, text) in enumerate(raw, start=1):
        if code_chars == 0 and text:
            kinds[idx] = "comment"
        elif code_chars == 0:
            kinds[idx] = "blank"
        else:
            kinds[idx] = "code"
        comment_text[idx] = text
    return kinds, comment_text


def ext_of(path):
    for ext in (".tsx", ".ts", ".go", ".sql", ".toml"):
        if path.endswith(ext):
            return ext
    return ""


def check_density(path, added, kinds, violations):
    non_blank = [ln for ln in added if kinds.get(ln) != "blank"]
    if len(non_blank) < DENSITY_FLOOR_LINES:
        return
    comment_count = sum(1 for ln in non_blank if kinds.get(ln) == "comment")
    ratio = comment_count / len(non_blank) * 100
    if ratio > DENSITY_LIMIT_PERCENT:
        first_line = min(non_blank)
        violations.append(
            (path, first_line, "density", f"{ratio:.1f}%", f"{DENSITY_LIMIT_PERCENT}%")
        )


def check_block_length(path, added, kinds, lines, is_new, violations):
    sorted_added = sorted(added)
    runs = []
    run_lines = []
    prev = None
    for ln in sorted_added:
        text = lines[ln - 1] if ln - 1 < len(lines) else ""
        if GO_BUILD_LINE_RE.match(text.strip()):
            continue
        if kinds.get(ln) == "comment":
            if prev is not None and ln - prev == 1 and run_lines:
                run_lines.append(ln)
            else:
                if run_lines:
                    runs.append(run_lines)
                run_lines = [ln]
            prev = ln
        else:
            if run_lines:
                runs.append(run_lines)
            run_lines = []
            prev = ln
    if run_lines:
        runs.append(run_lines)

    for run in runs:
        if len(run) <= BLOCK_LENGTH_LIMIT:
            continue
        if is_new and run[0] == 1:
            continue
        violations.append(
            (path, run[0], "block-length", str(len(run)), str(BLOCK_LENGTH_LIMIT))
        )


SENTENCE_SPLIT_RE = re.compile(r"(?<=[.!?])\s+")
NORMALIZE_STRIP_RE = re.compile(r"[^a-z0-9\s]")
WHITESPACE_RE = re.compile(r"\s+")


def normalize_sentence(sentence):
    lowered = sentence.lower()
    stripped = NORMALIZE_STRIP_RE.sub(" ", lowered)
    return WHITESPACE_RE.sub(" ", stripped).strip()


def collect_comment_groups(path, added, kinds, comment_text):
    sorted_added = sorted(added)
    groups = []
    run_lines = []
    run_texts = []
    prev = None
    for ln in sorted_added:
        text = comment_text.get(ln, "")
        if kinds.get(ln) == "comment" and text:
            if prev is not None and ln - prev == 1 and run_lines:
                run_lines.append(ln)
                run_texts.append(text)
            else:
                if run_lines:
                    groups.append((run_lines[0], " ".join(run_texts)))
                run_lines = [ln]
                run_texts = [text]
            prev = ln
        else:
            if run_lines:
                groups.append((run_lines[0], " ".join(run_texts)))
            run_lines = []
            run_texts = []
            prev = ln
            if text:
                groups.append((ln, text))
    if run_lines:
        groups.append((run_lines[0], " ".join(run_texts)))
    return groups


def check_duplication(path, added, kinds, comment_text, sentence_index):
    for first_line, text in collect_comment_groups(path, added, kinds, comment_text):
        for fragment in SENTENCE_SPLIT_RE.split(text):
            normalized = normalize_sentence(fragment)
            if len(normalized.split()) < DUPLICATION_MIN_WORDS:
                continue
            sentence_index.setdefault(normalized, []).append((path, first_line))


def main(argv):
    if len(argv) != 4:
        sys.stderr.write("usage: comment_density.py <repo-root> <base-ref> <head-ref>\n")
        return 2
    _, root, base, head = argv

    violations = []
    sentence_index = {}

    for path in changed_files(root, base, head):
        ext = ext_of(path)
        added = added_line_numbers(root, base, head, path)
        if not added:
            continue
        lines = file_lines(root, head, path)
        kinds, comment_text = classify_file(lines, ext)
        new_file = is_new_file(root, base, path)

        check_density(path, added, kinds, violations)
        check_block_length(path, added, kinds, lines, new_file, violations)
        check_duplication(path, added, kinds, comment_text, sentence_index)

    for normalized, sites in sentence_index.items():
        if len(sites) <= DUPLICATION_LIMIT:
            continue
        for path, line in sites:
            violations.append(
                (path, line, "duplication", str(len(sites)), str(DUPLICATION_LIMIT))
            )

    violations.sort(key=lambda v: (v[0], v[1], v[2]))
    for path, line, check, value, limit in violations:
        print(f"{path}:{line}: {check} {value} > {limit}")

    if violations:
        return 1
    print("comment-density check passed")
    return 0


if __name__ == "__main__":
    sys.exit(main(sys.argv))
