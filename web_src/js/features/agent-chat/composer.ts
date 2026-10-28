import {POST} from '../../modules/fetch.ts';
import {initPresets, readPresets} from './presets.ts';
import {initTaskTokens} from './task-tokens.ts';
import {svg} from '../../svg.ts';
import {createElementFromHTML, toggleElem} from '../../utils/dom.ts';

// how many files of one kind a list shows before the rest waits behind a single button;
// keep in step with chatAttachmentLimit in routers/web/agent/chat.go
const attachmentLimit = 5;

// the overflow of a row waits behind one control; the picture row's control is a tile that
// says how many it holds back, so that row shows one tile fewer
function limitAttachmentGroup(group: HTMLElement, moreLabel: string, counted = false) {
  const items = Array.from(group.children) as HTMLElement[];
  if (items.length <= attachmentLimit) return;
  const shown = counted ? attachmentLimit - 1 : attachmentLimit;
  for (const item of items.slice(shown)) toggleElem(item, false);
  const moreEl = document.createElement('button');
  moreEl.type = 'button';
  moreEl.className = counted ? 'agent-chat-attach-more agent-chat-attach-count' : 'agent-chat-attach-more';
  moreEl.textContent = counted ? `+${items.length - shown}` : moreLabel;
  group.append(moreEl);
}

// the button reveals what its own row kept back, then has nothing left to do
function expandAttachments(button: HTMLElement) {
  for (const item of button.parentElement?.querySelectorAll<HTMLElement>('.tw-hidden') ?? []) toggleElem(item, true);
  button.remove();
}

export function formatFileSize(bytes: number): string {
  const units = ['B', 'KB', 'MB'];
  let value = bytes;
  let unit = 0;
  while (value >= 1024 && unit < units.length - 1) {
    value /= 1024;
    unit++;
  }
  return `${unit === 0 ? value : value.toFixed(1)} ${units[unit]}`;
}

// The browser gives no sign that a file is chosen, so the composer lists what will go with
// the message and lets the reader take one back. The list is cleared once the files are sent.
// A change never replaces what is already there: picking again adds to the message.
export function initAttachmentList(form: HTMLFormElement): () => void {
  const input = form.querySelector<HTMLInputElement>('input[type="file"][name="files"]');
  const list = form.querySelector<HTMLElement>('.agent-chat-attach-list');
  const chatEl = document.querySelector<HTMLElement>('.agent-chat');
  const removeLabel = chatEl?.getAttribute('data-attach-remove-label') ?? '';
  const moreLabel = chatEl?.getAttribute('data-attach-more-label') ?? '';
  if (!input || !list) return () => {};
  const chosen: File[] = []; // the picker replaces input.files, so the selection has to live here
  let suppress = false; // removing re-dispatches change; the handler must not merge that again
  const syncFiles = () => {
    const merged = new DataTransfer();
    for (const file of chosen) merged.items.add(file);
    input.files = merged.files;
  };

  const removeAt = (index: number) => {
    chosen.splice(index, 1);
    syncFiles();
    suppress = true;
    try {
      input.dispatchEvent(new Event('change', {bubbles: false})); // the list draws itself again
    } finally {
      suppress = false;
    }
  };
  const chip = (file: File, index: number, picture: boolean): HTMLElement => {
    const thumb = picture ? URL.createObjectURL(file) : '';
    const body = thumb ?
      `<img class="agent-chat-attach-thumb" alt="" src="${thumb}">` :
      svg('octicon-paperclip', 14);
    const el = createElementFromHTML<HTMLElement>(
      `<div class="agent-chat-attachment">${body}<span class="gt-ellipsis"></span>` +
      `<span class="agent-chat-attachment-size"></span>` +
      `<button type="button" class="agent-chat-attach-remove" data-tooltip-content="${removeLabel}" aria-label="${removeLabel}">${svg('octicon-x', 14)}</button></div>`,
    );
    el.querySelector('span')!.textContent = file.name;
    el.querySelector('.agent-chat-attachment-size')!.textContent = formatFileSize(file.size);
    el.querySelector('button')!.addEventListener('click', () => removeAt(index));
    return el;
  };

  const render = () => {
    const files = Array.from(input.files!);
    toggleElem(list, files.length > 0);
    const picturesEl = document.createElement('div');
    picturesEl.className = 'agent-chat-attach-group agent-chat-attach-pictures';
    const filesEl = document.createElement('div');
    filesEl.className = 'agent-chat-attach-group agent-chat-attach-files';
    for (const [index, file] of files.entries()) {
      const picture = file.type.startsWith('image/');
      (picture ? picturesEl : filesEl).append(chip(file, index, picture));
    }
    const groups = [picturesEl, filesEl].filter((group) => group.childElementCount > 0);
    for (const group of groups) limitAttachmentGroup(group, moreLabel);
    list.replaceChildren(...groups);
  };
  input.addEventListener('change', () => {
    // files may be picked in several rounds: append to the message, never overwrite
    const picked = Array.from(input.files!);
    if (!suppress && picked.length) {
      chosen.push(...picked);
      syncFiles();
    }
    render();
  });

  const clear = () => {
    chosen.length = 0;
    input.value = '';
    render();
  };
  form.addEventListener('click', (event) => {
    const button = (event.target as HTMLElement).closest<HTMLElement>('.agent-chat-attach-more');
    if (button) expandAttachments(button);
  });
  return clear;
}

// ChatGPT and Claude send on Enter and grow the input while typing; a chat box that stays
// three rows tall wastes half the panel.
export function initComposerKeys(form: HTMLFormElement, input: HTMLTextAreaElement, presets?: {keyDown: (event: KeyboardEvent) => boolean}) {
  input.addEventListener('keydown', (event) => {
    if (event.isComposing) return;
    if (presets?.keyDown(event)) { // the open palette owns the arrow keys and Enter
      event.preventDefault();
      return;
    }
    if (event.key !== 'Enter' || event.shiftKey) return;
    event.preventDefault();
    form.requestSubmit();
  });
  const grow = () => {
    input.style.height = 'auto';
    input.style.height = `${Math.min(input.scrollHeight, 240)}px`;
  };
  input.addEventListener('input', grow);
  grow();
}

// a conversation does not exist until its first message, so the draft posts the message and
// then follows the created conversation
export function initDraftComposer() {
  const form = document.querySelector<HTMLFormElement>('.agent-chat-composer[data-draft-url]');
  if (!form) return;
  const button = form.querySelector<HTMLButtonElement>('button[type="submit"]')!;
  const input = form.querySelector<HTMLTextAreaElement>('.agent-chat-input')!;
  const presets = readPresets(form);
  initAttachmentList(form);
  initTaskTokens(form, input, presets);
  initComposerKeys(form, input, initPresets(form, input));

  form.addEventListener('submit', async (event) => {
    event.preventDefault();
    button.classList.add('disabled');
    try {
      const resp = await POST(form.getAttribute('data-draft-url')!, {
        data: new FormData(form),
        headers: {'X-Requested-With': 'XMLHttpRequest'},
      });
      const location = resp.headers.get('location');
      if (resp.ok && location) {
        window.location.assign(location);
        return;
      }
    } catch {
      // fall through to re-enable the button
    }
    button.classList.remove('disabled');
  });
}

export function fitLayout() {
  const chat = document.querySelector<HTMLElement>('.agent-chat');
  const footer = document.querySelector<HTMLElement>('footer');
  if (!chat || !footer) return;
  const top = chat.getBoundingClientRect().top + window.scrollY;
  // the page owns the viewport, so the footer sits at its bottom and the panel fills the rest
  const height = `${Math.max(240, Math.round(innerHeight - top - footer.offsetHeight - 8))}px`;
  if (chat.style.height !== height) chat.style.height = height; // writing the same value loops the observer
}
