import {formatFileSize, initAttachmentList} from './agent-chat/composer.ts';
import {initComposerInput} from './agent-chat/input.ts';

test('formatFileSize', () => {
  expect(formatFileSize(0)).toBe('0 B');
  expect(formatFileSize(8)).toBe('8 B');
  expect(formatFileSize(1023)).toBe('1023 B');
  expect(formatFileSize(1024)).toBe('1.0 KB');
  expect(formatFileSize(1536)).toBe('1.5 KB');
  expect(formatFileSize(1024 * 1024)).toBe('1.0 MB');
});

test('attachment list appends picks, removes and clears without duplicating', () => {
  document.body.innerHTML = `
    <form class="agent-chat-composer">
      <input type="file" name="files" multiple>
      <div class="agent-chat-attach-list tw-hidden"></div>
    </form>`;
  const form = document.querySelector<HTMLFormElement>('.agent-chat-composer')!;
  const clear = initAttachmentList(form);
  const input = form.querySelector<HTMLInputElement>('input[type="file"]')!;
  const list = form.querySelector<HTMLElement>('.agent-chat-attach-list')!;
  const pick = (...files: File[]) => {
    const transfer = new DataTransfer();
    for (const file of files) transfer.items.add(file);
    input.files = transfer.files;
    input.dispatchEvent(new Event('change'));
  };
  const sent = () => Array.from(input.files!, (file) => file.name);
  const chips = () => Array.from(list.querySelectorAll('.agent-chat-attachment'), (chip) => chip.querySelector('span')!.textContent);

  pick(new File(['a'], 'a.txt', {type: 'text/plain'}), new File(['b'], 'b.txt', {type: 'text/plain'}));
  expect(chips()).toEqual(['a.txt', 'b.txt']);
  expect(sent()).toEqual(['a.txt', 'b.txt']);
  expect(list.querySelectorAll('img.agent-chat-attach-thumb')).toHaveLength(0);

  pick(new File([new Uint8Array([137, 80, 78, 71])], 'c.png', {type: 'image/png'}));
  expect(chips()).toEqual(['c.png', 'a.txt', 'b.txt']); // a second pick adds to the message, pictures before files
  expect(sent()).toEqual(['a.txt', 'b.txt', 'c.png']);
  expect(list.querySelectorAll('img.agent-chat-attach-thumb')).toHaveLength(1); // pictures get a thumbnail

  list.querySelectorAll<HTMLButtonElement>('.agent-chat-attach-remove')[1].click();
  expect(chips()).toEqual(['c.png', 'b.txt']);
  expect(sent()).toEqual(['b.txt', 'c.png']); // taking one back keeps the rest

  clear();
  expect(chips()).toEqual([]);
  expect(sent()).toEqual([]);
  expect(list.classList.contains('tw-hidden')).toBe(true);
});

test('the box reads a task word as its name while the message keeps the word', () => {
  document.body.innerHTML = `
    <form class="agent-chat-composer">
      <div class="agent-chat-input" contenteditable="true"></div>
      <input type="hidden" name="content">
    </form>`;
  const form = document.querySelector<HTMLFormElement>('.agent-chat-composer')!;
  const box = form.querySelector<HTMLElement>('.agent-chat-input')!;
  const field = form.querySelector<HTMLInputElement>('input[name="content"]')!;
  const input = initComposerInput(form)!;
  const known = (word: string) => (word === '/issue-triage' ? {display: '이슈 검토', canonical: word} : null);
  const chip = () => box.querySelector<HTMLElement>('.agent-chat-token');

  input.setText('/issue-triage 이슈 #1');
  expect(field.value).toBe('/issue-triage 이슈 #1');
  expect(box.getAttribute('data-empty')).toBe('false');

  input.applyTokens(known);
  expect(chip()!.textContent).toBe('이슈 검토');
  expect(chip()!.getAttribute('data-canonical')).toBe('/issue-triage');
  expect(box.textContent).toBe('이슈 검토 이슈 #1'); // what the reader reads
  expect(field.value).toBe('/issue-triage 이슈 #1'); // what the message carries

  // a word this page knows nothing about stays the reader's own text
  input.setText('/no-such-skill ');
  input.applyTokens(known);
  expect(chip()).toBeNull();
  expect(field.value).toBe('/no-such-skill ');

  // the word the reader just finished keeps the space it was broken with, and the caret
  input.setText('/code ');
  const text = box.firstChild as Text;
  const range = document.createRange();
  range.setStart(text, text.length);
  range.collapse(true);
  getSelection()!.removeAllRanges();
  getSelection()!.addRange(range);
  input.replaceBefore('/code'.length, {display: '코드 검토', canonical: '/code'}, 1);
  expect(chip()!.textContent).toBe('코드 검토');
  expect(box.textContent).toBe('코드 검토 '); // the space stands where the reader left it
  expect(field.value).toBe('/code ');

  input.clear();
  expect(field.value).toBe('');
  expect(box.getAttribute('data-empty')).toBe('true'); // the placeholder has its place again
});
