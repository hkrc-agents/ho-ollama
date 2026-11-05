import test from 'node:test';
import assert from 'node:assert/strict';
import { parseCommand } from '../entry/src/main/ets/services/CommandLine.ts';

test('ordinary command and whitespace', () => {
  assert.deepEqual(parseCommand('  ollama\tlist  '), ['ollama', 'list']);
});
test('quoted prompt and empty arguments', () => {
  assert.deepEqual(parseCommand('llama-cli -p "What is 2 + 3?" --empty \'\''),
    ['llama-cli', '-p', 'What is 2 + 3?', '--empty', '']);
});
test('escapes and single-quote literals', () => {
  assert.deepEqual(parseCommand('ollama show a\\ b "c\\\"d" \'a\\b\''),
    ['ollama', 'show', 'a b', 'c"d', 'a\\b']);
});
test('environment and globs are literal, never expanded', () => {
  assert.deepEqual(parseCommand('ollama show "$HOME" *.gguf'), ['ollama', 'show', '$HOME', '*.gguf']);
});
test('quoted operators remain prompt text', () => {
  assert.deepEqual(parseCommand('llama-cli -p "a | b; c > d"'), ['llama-cli', '-p', 'a | b; c > d']);
});
for (const [name, command] of [
  ['empty', ' '], ['unclosed quote', 'ollama "list'], ['trailing escape', 'ollama list\\'],
  ['pipe', 'ollama list | cat'], ['redirect', 'ollama list > file'],
  ['chained', 'ollama list && ollama ps'], ['newline', 'ollama list\nollama ps'],
  ['NUL', 'ollama\0list'], ['too many arguments', 'ollama '+Array(32).fill('x').join(' ')],
  ['too long', 'x'.repeat(8193)],
]) {
  test('rejects '+name, () => assert.throws(() => parseCommand(command)));
}
