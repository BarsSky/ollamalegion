import re, sys
src = open(sys.argv[1], 'r', encoding='utf-8').read()
# Strip comments and strings
src = re.sub(r'//[^\n]*', '', src)
src = re.sub(r'/\*[\s\S]*?\*/', '', src)
src = re.sub(r'"(?:[^"\\]|\\.)*"', '""', src)
src = re.sub(r"'(?:[^'\\]|\\.)*'", "''", src)
src = re.sub(r'`(?:[^`\\]|\\.)*`', '``', src, flags=re.DOTALL)

depth = 0
for i, ch in enumerate(src):
    if ch == '{':
        depth += 1
    elif ch == '}':
        depth -= 1
        if depth < 0:
            line = src[:i].count('\n') + 1
            print(f'Unbalanced }} at char {i}, line {line}')
            break
print(f'Final brace depth: {depth}')
print(f'Length: {len(src)}')
