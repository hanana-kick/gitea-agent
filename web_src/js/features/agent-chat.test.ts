import {formatFileSize, initAttachmentList} from './agent-chat/composer.ts';

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
