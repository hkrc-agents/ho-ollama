import test from 'node:test';
import assert from 'node:assert/strict';
import { insertNewLine } from '../entry/src/main/ets/services/InputEditing.ts';

test('empty input', () => assert.deepEqual(insertNewLine('', 0, 0), {text:'\n',caret:1}));
test('end', () => assert.deepEqual(insertNewLine('abc', 3, 3), {text:'abc\n',caret:4}));
test('middle', () => assert.deepEqual(insertNewLine('abcd', 2, 2), {text:'ab\ncd',caret:3}));
test('replace selection', () => assert.deepEqual(insertNewLine('abcd', 1, 3), {text:'a\nd',caret:2}));
test('backward selection', () => assert.deepEqual(insertNewLine('abcd', 3, 1), {text:'a\nd',caret:2}));
test('clamp bounds', () => assert.deepEqual(insertNewLine('abc', -1, 8), {text:'\n',caret:1}));
test('UTF16 caret', () => assert.deepEqual(insertNewLine('你😀好', 3, 3), {text:'你😀\n好',caret:4}));
