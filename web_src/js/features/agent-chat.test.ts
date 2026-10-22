import {formatFileSize} from './agent-chat.ts';

test('formatFileSize', () => {
  expect(formatFileSize(0)).toBe('0 B');
  expect(formatFileSize(8)).toBe('8 B');
  expect(formatFileSize(1023)).toBe('1023 B');
  expect(formatFileSize(1024)).toBe('1.0 KB');
  expect(formatFileSize(1536)).toBe('1.5 KB');
  expect(formatFileSize(1024 * 1024)).toBe('1.0 MB');
});
