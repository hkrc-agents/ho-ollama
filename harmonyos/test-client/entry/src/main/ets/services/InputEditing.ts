export interface EditedInput {
  text: string;
  caret: number;
}

// ArkUI selection offsets and JS strings use UTF-16 indices.
export function insertNewLine(text: string, start: number, end: number): EditedInput {
  const left = Math.max(0, Math.min(text.length, Math.min(start, end)));
  const right = Math.max(left, Math.min(text.length, Math.max(start, end)));
  return { text: text.slice(0, left) + '\n' + text.slice(right), caret: left + 1 };
}
