// Argument splitting only; never implicitly wrap the line in sh/eval.
// The user may explicitly choose an installed shell executable like any tool.
export function parseCommand(line: string): string[] {
  if (line.length > 8192) throw new Error('Command is too long');
  const args: string[] = [];
  let word = '';
  let quote = '';
  let escaped = false;
  let started = false;
  for (let i = 0; i < line.length; i++) {
    const c = line.charAt(i);
    if (c === '\n' || c === '\r' || c === '\0') throw new Error('Use the stdin field for multiple lines');
    if (escaped) { word += c; escaped = false; started = true; continue; }
    if (c === '\\' && quote !== "'") { escaped = true; started = true; continue; }
    if (quote !== '') {
      if (c === quote) quote = ''; else word += c;
      continue;
    }
    if (c === '"' || c === "'") { quote = c; started = true; continue; }
    if ('|&;<>'.includes(c)) throw new Error('Shell operators are not supported');
    if (c === ' ' || c === '\t') {
      if (started) { args.push(word); word = ''; started = false; }
    } else { word += c; started = true; }
  }
  if (escaped || quote !== '') throw new Error('Unclosed quote or trailing escape');
  if (started) args.push(word);
  if (args.length === 0 || args.length > 32) throw new Error('Enter one command with at most 32 arguments');
  return args;
}
