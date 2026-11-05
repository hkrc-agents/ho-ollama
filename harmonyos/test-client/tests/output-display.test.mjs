import test from 'node:test';
import assert from 'node:assert/strict';
import { completeUtf8Length, displayOutput } from '../entry/src/main/ets/services/OutputDisplay.ts';

test('Chinese, emoji and combining marks stay intact', () => {
  const text = '你好😀 e\u0301\n第二行';
  assert.equal(displayOutput(text), text);
});
test('every byte split preserves valid UTF8 prefixes', () => {
  const bytes = new TextEncoder().encode('a你😀好');
  for (let n = 0; n <= bytes.length; n++) {
    const part = bytes.slice(0, n);
    const decoded = new TextDecoder('utf-8', {fatal:true}).decode(part.slice(0, completeUtf8Length(part)));
    assert.ok('a你😀好'.startsWith(decoded));
  }
});
test('invalid bytes are not silently removed', () => {
  const bytes = new Uint8Array([0xff, 0x80]);
  assert.equal(completeUtf8Length(bytes), bytes.length);
  assert.equal(new TextDecoder().decode(bytes), '��');
});
test('Ollama progress cursor modes and erase', () => {
  assert.equal(displayOutput('\x1b[?2026h\x1b[?25l⠋\x1b[1G⠙\x1b[K\x1b[?25h\x1b[?2026l\x1b[2K\x1b[1G'), '');
});
test('erase and cursor left retain replacement text', () => {
  assert.equal(displayOutput('abcdef\x1b[3D\x1b[K中文'), 'abc中文');
  assert.equal(displayOutput('abc\x1b[2DXY'), 'aXY');
});
test('CR, backspace, tabs and ordinary newlines', () => {
  assert.equal(displayOutput('abc\rX\bY\n1\t2'), 'Ybc\n1       2');
});
test('multi-line progress rewrite', () => {
  assert.equal(displayOutput('old\nnext\x1b[A\x1b[2K\x1b[1Gnew'), 'new\nnext');
});
test('OSC URLs and title payloads stay out of display', () => {
  assert.equal(displayOutput('\x1b]8;;https://example.com\x1b\\link\x1b]8;;\x1b\\'), 'link');
  assert.equal(displayOutput('a\x1b]0;title\x07b'), 'ab');
});
test('incomplete escapes are withheld until next cumulative snapshot', () => {
  for (const tail of ['\x1b', '\x1b[', '\x1b[?25', '\x1b]title']) {
    assert.equal(displayOutput('中文'+tail), '中文');
  }
});
test('large cursor motions have bounded cost', () => {
  assert.equal(displayOutput('\x1b[9999999999C!').length, 4097);
});
test('ordinary long lines are not clipped at cursor-motion limit', () => {
  const text = 'x'.repeat(9000);
  assert.equal(displayOutput(text), text);
});

test('charset designations and other ESC intermediate sequences do not leak text', () => {
  assert.equal(displayOutput('\x1b(B中文\x1b)0😀\x1b%G!'), '中文😀!');
  for (const tail of ['\x1b(', '\x1b)', '\x1b%']) {
    assert.equal(displayOutput('你好'+tail), '你好');
  }
});
test('C1 CSI and string controls have the same display semantics as ESC forms', () => {
  assert.equal(displayOutput('old\u009b2K\u009b1G新'), '新');
  assert.equal(displayOutput('a\u009d0;hidden\u009cb'), 'ab');
  assert.equal(displayOutput('a\x1b]0;hidden\u009cb'), 'ab');
  assert.equal(displayOutput('a\u0090hidden\u009cb'), 'ab');
  assert.equal(displayOutput('a\x1bXhidden\x1b\\b'), 'ab');
  assert.equal(displayOutput('a\u0085b'), 'a\nb');
});
test('a cancelled string resumes visible text without showing its payload', () => {
  assert.equal(displayOutput('a\x1b]hidden\x18b'), 'ab');
  assert.equal(displayOutput('a\x1bPhidden\x1ab'), 'ab');
});
test('only potentially valid incomplete UTF8 is withheld', () => {
  for (const values of [[0xe0,0x80], [0xed,0xa0], [0xf0,0x80], [0xf4,0x90]]) {
    const bytes = new Uint8Array(values);
    assert.equal(completeUtf8Length(bytes), bytes.length);
  }
  for (const values of [[0xe0,0xa0], [0xed,0x9f], [0xf0,0x90,0x80], [0xf4,0x8f]]) {
    assert.equal(completeUtf8Length(new Uint8Array(values)), 0);
  }
});
