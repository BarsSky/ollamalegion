// check_braces.js — proper JS parser-aware brace/paren counter
const fs = require('fs');
const src = fs.readFileSync(process.argv[2], 'utf-8');

let depthCurly = 0, depthParen = 0, depthBracket = 0;
let inString = null;  // ', ", `, or null
let inLineComment = false;
let inBlockComment = false;
let i = 0;
let lastError = null;
const lines = src.split('\n');

for (let lineIdx = 0; lineIdx < lines.length; lineIdx++) {
    const line = lines[lineIdx];
    for (let j = 0; j < line.length; j++) {
        const ch = line[j];
        const prev = j > 0 ? line[j-1] : '';
        const next = j + 1 < line.length ? line[j+1] : '';

        if (inLineComment) continue;
        if (inBlockComment) {
            if (ch === '*' && next === '/') { inBlockComment = false; j++; }
            continue;
        }
        if (inString) {
            if (ch === '\\') { j++; continue; }  // skip escaped char
            if (ch === inString) inString = null;
            continue;
        }

        if (ch === '/' && next === '/') { inLineComment = true; continue; }
        if (ch === '/' && next === '*') { inBlockComment = true; j++; continue; }
        if (ch === '"' || ch === "'" || ch === '`') { inString = ch; continue; }
        if (ch === '{') depthCurly++;
        else if (ch === '}') { depthCurly--; if (depthCurly < 0 && !lastError) lastError = `Extra } at line ${lineIdx+1}: ${line.slice(0,80)}`; }
        else if (ch === '(') depthParen++;
        else if (ch === ')') { depthParen--; if (depthParen < 0 && !lastError) lastError = `Extra ) at line ${lineIdx+1}: ${line.slice(0,80)}`; }
        else if (ch === '[') depthBracket++;
        else if (ch === ']') { depthBracket--; if (depthBracket < 0 && !lastError) lastError = `Extra ] at line ${lineIdx+1}: ${line.slice(0,80)}`; }
    }
    inLineComment = false;
}

console.log('Final: curly=' + depthCurly + ' paren=' + depthParen + ' bracket=' + depthBracket);
if (lastError) console.log('Error: ' + lastError);
if (inString) console.log('Unclosed string: ' + inString);
if (inBlockComment) console.log('Unclosed block comment');
