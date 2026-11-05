// Display-only normalization. Captured byte streams remain unchanged.
export function completeUtf8Length(bytes: Uint8Array): number {
  if (bytes.length === 0) return 0;
  let start = bytes.length - 1;
  while (start > 0 && (bytes[start] & 0xc0) === 0x80) start--;
  const lead = bytes[start];
  const need = lead >= 0xc2 && lead <= 0xdf ? 2 :
    lead >= 0xe0 && lead <= 0xef ? 3 : lead >= 0xf0 && lead <= 0xf4 ? 4 : 1;
  // An impossible prefix is malformed input, not a split valid character.
  // Let the decoder expose it as U+FFFD rather than hiding it until job exit.
  if (bytes.length > start + 1) {
    const second = bytes[start + 1];
    if ((lead === 0xe0 && second < 0xa0) || (lead === 0xed && second > 0x9f) ||
        (lead === 0xf0 && second < 0x90) || (lead === 0xf4 && second > 0x8f)) return bytes.length;
  }
  return bytes.length - start < need ? start : bytes.length;
}

export function displayOutput(text: string): string {
  const lines: string[][] = [[]];
  let row = 0;
  let column = 0;
  let i = 0;
  while (i < text.length) {
    let code = text.charCodeAt(i++);
    if (code === 0x1b || code === 0x9b || code === 0x9d || code === 0x90 ||
        code === 0x98 || code === 0x9e || code === 0x9f) {
      let kind = '';
      if (code === 0x1b) {
        if (i >= text.length) break;
        kind = text[i++];
      } else {
        kind = code === 0x9b ? '[' : code === 0x9d ? ']' : code === 0x90 ? 'P' :
          code === 0x98 ? 'X' : code === 0x9e ? '^' : '_';
      }
      if (kind === '[') {
        const begin = i;
        while (i < text.length && !(text.charCodeAt(i) >= 0x40 && text.charCodeAt(i) <= 0x7e)) i++;
        if (i >= text.length) break;
        const command = text[i++];
        const params = text.slice(begin, i - 1);
        // Private modes, colors and unsupported terminal commands have no text.
        if (!/^[0-9;]*$/.test(params)) continue;
        const values = params.split(';');
        const n = Math.min(4096, Math.max(1, Number(values[0]) || 1));
        if (command === 'D') column = Math.max(0, column - n);
        else if (command === 'C') column = Math.min(4096, column + n);
        else if (command === 'G') column = n - 1;
        else if (command === 'A') row = Math.max(0, row - n);
        else if (command === 'B') row = Math.min(lines.length - 1, row + n);
        else if (command === 'K') {
          const mode = Number(values[0]) || 0;
          if (mode === 0) lines[row].splice(column);
          else if (mode === 1) {
            for (let k = 0; k <= column; k++) lines[row][k] = ' ';
          } else if (mode === 2) lines[row] = [];
        } else if (command === 'J' && Number(values[0]) === 2) {
          lines.splice(0, lines.length, []); row = 0; column = 0;
        }
      } else if (kind === ']' || kind === 'P' || kind === 'X' || kind === '^' || kind === '_') {
        // String controls terminate at ST, cancellation, or (for OSC) BEL.
        while (i < text.length) {
          const end = text.charCodeAt(i++);
          if (end === 0x9c || end === 0x18 || end === 0x1a || (kind === ']' && end === 7)) break;
          if (end === 0x1b && text[i] === '\\') { i++; break; }
        }
      } else if (kind.charCodeAt(0) >= 0x20 && kind.charCodeAt(0) <= 0x2f) {
        // ESC intermediates plus a final byte, e.g. ESC ( B / ESC % G.
        // In cumulative snapshots an unfinished sequence remains withheld.
        while (i < text.length && text.charCodeAt(i) >= 0x20 && text.charCodeAt(i) <= 0x2f) i++;
        if (i < text.length && text.charCodeAt(i) >= 0x30 && text.charCodeAt(i) <= 0x7e) i++;
      }
      continue;
    }
    if (code === 0x85) code = 10; // C1 NEL, not printable text.
    if (code >= 0x80 && code <= 0x9f) continue;
    if (code === 13) { column = 0; continue; }
    if (code === 10) {
      row++; column = 0;
      if (row === lines.length) lines.push([]);
      continue;
    }
    if (code === 8) { column = Math.max(0, column - 1); continue; }
    if (code === 9) {
      const end = Math.min(4096, (Math.floor(column / 8) + 1) * 8);
      while (column < end) lines[row][column++] = ' ';
      continue;
    }
    if (code < 32 || code === 127) continue;
    let ch = String.fromCharCode(code);
    if (code >= 0xd800 && code <= 0xdbff && i < text.length) {
      const next = text.charCodeAt(i);
      if (next >= 0xdc00 && next <= 0xdfff) ch += text[i++];
    }
    if (column < 262144) {
      while (lines[row].length < column) lines[row].push(' ');
      lines[row][column++] = ch;
    }
  }
  const result: string[] = [];
  for (const line of lines) result.push(line.join(''));
  return result.join('\n');
}
