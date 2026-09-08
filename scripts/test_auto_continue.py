#!/usr/bin/env python3
"""Standalone test for R60.21 auto_continue detection logic.

Mirrors the Go test cases without needing cgo build.

Validates:
- detectIncompleteResponse — odd backticks, mid-line cutoff
- buildContinuePrompt — has all required components
- truncateReason — returns correct reason string
"""
import sys

def detect_incomplete_response(content: str) -> bool:
    """Pure Python port of Go's detectIncompleteResponse.

    Heuristics (in priority order):
    1. Odd ``` count → unclosed code block
    2. Last non-empty line ends with continuation char
    """
    if not content:
        return False

    # 1. Unclosed code block: odd number of ```
    if content.count("```") % 2 != 0:
        return True

    # 2. Mid-line cutoff
    trimmed = content.rstrip(" \t\n\r")
    if not trimmed:
        return False

    last_newline = trimmed.rfind("\n")
    last_line = trimmed[last_newline + 1:] if last_newline != -1 else trimmed
    # rstrip the last line too — otherwise trailing whitespace
    # masks continuation chars like "= " at the end.
    last_line = last_line.rstrip(" \t")

    # Skip if last line is a "normal" closing
    if ends_with_complete_statement(last_line):
        return False

    # Check for continuation chars (only first char after rstrip)
    if last_line and last_line[-1] in "=(,:":
        return True

    if not last_line.strip():
        return True

    if len(last_line) > 200 and not ends_with_complete_statement(last_line):
        return True

    return False


def ends_with_complete_statement(line: str) -> bool:
    if not line:
        return True
    last = line[-1]
    return last in ".!?)]}'\"`"


def build_continue_prompt(partial_content: str, reason: str) -> str:
    """Pure Python port of Go's buildContinuePrompt."""
    parts = ["Продолжи точно с того места, где остановился. "]
    if reason == "unclosed_code_block":
        parts.append("Кодовый блок остался незакрытым (нет финальной строки ```). ")
        parts.append("Сгенерируй ТОЛЬКО продолжение кода, начиная с последнего выведенного символа. ")
        parts.append("ОБЯЗАТЕЛЬНО закрой блок тремя обратными апострофами (```) в конце.")
    elif reason == "mid_line_cutoff":
        parts.append("Последняя строка кода обрезана посередине. ")
        parts.append("Сгенерируй ТОЛЬКО продолжение с места обрыва. ")
        parts.append("Не повторяй уже написанное.")
    else:
        parts.append("Сгенерируй ТОЛЬКО продолжение с места обрыва. ")
        parts.append("Не повторяй уже написанное.")

    anchor = partial_content[-200:] if len(partial_content) > 200 else partial_content
    parts.append("\n\nПоследние 200 символов твоего ответа (для контекста):\n```\n...")
    parts.append(anchor)
    parts.append("\n```")
    return "".join(parts)


def truncate_reason(content: str) -> str:
    if not content:
        return "empty"
    if content.count("```") % 2 != 0:
        return "unclosed_code_block"
    trimmed = content.rstrip(" \t\n\r")
    if not trimmed:
        return "empty"
    last_newline = trimmed.rfind("\n")
    last_line = trimmed[last_newline + 1:] if last_newline != -1 else trimmed
    last_line = last_line.rstrip(" \t")
    if ends_with_complete_statement(last_line):
        return ""
    if last_line and last_line[-1] in "=(,:":
        return "mid_line_cutoff"
    return ""


# ===== TESTS =====

def test_unclosed_code_block():
    cases = [
        ("even backticks (closed)", "```python\nimport os\n```\nDone.", False),
        ("odd backticks (unclosed)", "```python\nimport os\nprint('hi')", True),
        ("three blocks all closed", "```python\nx = 1\n```\ntext\n```js\ny = 2\n```", False),
        ("two opened, one closed", "```python\nx = 1\n```\ntext\n```js\ny = 2", True),
        ("no backticks", "Just plain text without any code block.", False),
    ]
    failed = 0
    for name, content, want in cases:
        got = detect_incomplete_response(content)
        status = "OK" if got == want else "FAIL"
        if got != want:
            failed += 1
        print(f"  [{status}] {name}: got={got} want={want}")
    return failed


def test_mid_line_cutoff():
    cases = [
        ("ends with =", "```python\nt_values = ", True),
        ("ends with (", "```python\nprint('hi", True),
        ("ends with {", "```python\nx = {", True),
        ("ends with [", "```python\narr = [", True),
        ("ends with ,", "```python\nprint(1, 2,", True),
        ("ends with . (unclosed block — True)", "```python\nprint('hi.')", True),
        ("ends with :", "```python\nif x > 0:", True),
        ("ends with newline (unclosed block — True)", "```python\nx = 1\n", True),
        ("ends with newline + spaces", "```python\nx = 1\n   ", True),
    ]
    failed = 0
    for name, content, want in cases:
        got = detect_incomplete_response(content)
        status = "OK" if got == want else "FAIL"
        if got != want:
            failed += 1
        print(f"  [{status}] {name}: got={got} want={want}")
    return failed


def test_plain_text():
    cases = [
        ("plain text ending with period", "The answer is 42. The formula is correct.", False),
        ("plain text ending with newline", "This is a complete sentence.\n", False),
        ("bullet list ending cleanly", "- Item 1\n- Item 2\n- Item 3", False),
        ("empty content", "", False),
        ("only whitespace", "   \n\n  ", False),
    ]
    failed = 0
    for name, content, want in cases:
        got = detect_incomplete_response(content)
        status = "OK" if got == want else "FAIL"
        if got != want:
            failed += 1
        print(f"  [{status}] {name}: got={got} want={want}")
    return failed


def test_r60_20_real_cases():
    cases = [
        "```python\nimport numpy as np\nimport matplotlib.pyplot as plt\n" +
        "def simulate_projectile_motion(v0, theta_deg, g=9.81, ...):\n" +
        "    # Инициальные условия\n" +
        "    x, y = 0.0, 0.0\n" +
        "    vx, vy = v0 * np.cos(theta_rad), v0 * np.sin(theta_rad)\n" +
        "    \n" +
        "    # Время и массивы для хранения данных\n" +
        "    times = ",
        "...\n    time = 0.0\n    t_values = ",
        "...\n    # Временные данные\n    t = ",
        "...\n    vx, vy = vx0, vy0\n        \n        trajectory = ",
        "1. Установил ли ты 'scipy' и 'matplotlib'?\n```bash\npip install numpy matplotlib scipy",
    ]
    failed = 0
    for i, content in enumerate(cases):
        got = detect_incomplete_response(content)
        name = f"r60_20_case_{chr(65+i)}"
        status = "OK" if got else "FAIL"
        if not got:
            failed += 1
        print(f"  [{status}] {name}: got={got} (expected true)")
    return failed


def test_build_continue_prompt():
    prompt = build_continue_prompt(
        "```python\nimport numpy as np\nv0 = 50.0\n",
        "unclosed_code_block",
    )
    checks = [
        ("mentions ```", "```" in prompt),
        ("mentions Продолжи", "Продолжи" in prompt),
        ("includes partial content", "import numpy as np" in prompt),
        ("mentions closing fence", "закрой" in prompt),
    ]
    failed = 0
    for name, expected in checks:
        status = "OK" if expected else "FAIL"
        if not expected:
            failed += 1
        print(f"  [{status}] {name}: {expected}")
    print(f"  Prompt preview: {prompt[:200]}")
    return failed


def test_truncate_reason():
    cases = [
        ("```python\nx = 1\n", "unclosed_code_block"),
        ("```python\nt = ", "unclosed_code_block"),  # unclosed block takes priority over mid-line
        ("Hello world.", ""),
        ("", "empty"),
        ("```python\nx = 1\n```\nDone.", ""),
    ]
    failed = 0
    for content, want in cases:
        got = truncate_reason(content)
        status = "OK" if got == want else "FAIL"
        if got != want:
            failed += 1
        print(f"  [{status}] {repr(content)[:50]:50s} → {got!r} (want {want!r})")
    return failed


def main():
    total_failed = 0

    print("=== detectIncompleteResponse — unclosed code block ===")
    total_failed += test_unclosed_code_block()
    print()
    print("=== detectIncompleteResponse — mid-line cutoff ===")
    total_failed += test_mid_line_cutoff()
    print()
    print("=== detectIncompleteResponse — plain text ===")
    total_failed += test_plain_text()
    print()
    print("=== detectIncompleteResponse — R60.20 real cases ===")
    total_failed += test_r60_20_real_cases()
    print()
    print("=== buildContinuePrompt ===")
    total_failed += test_build_continue_prompt()
    print()
    print("=== truncateReason ===")
    total_failed += test_truncate_reason()

    print()
    if total_failed == 0:
        print(f"=== ALL TESTS PASSED ===")
        sys.exit(0)
    else:
        print(f"=== {total_failed} TEST(S) FAILED ===")
        sys.exit(1)


if __name__ == "__main__":
    main()
