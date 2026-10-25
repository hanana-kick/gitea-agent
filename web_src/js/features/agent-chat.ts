import {GET, POST} from '../modules/fetch.ts';
import {copyToClipboardWithFeedback} from '../modules/clipboard.ts';
import {svg} from '../svg.ts';
import {errorMessage} from '../modules/errors.ts';
import {addDelegatedEventListener, createElementFromHTML, toggleElem} from '../utils/dom.ts';
import {followAgentLive, markSessionBusy, runIsBusy} from '../modules/agent-live.ts';
import type {AgentApproval, AgentConversation, AgentDelta, AgentMessage, AgentRun} from '../modules/agent-live.ts';

type AgentChatLabels = {queued: string, running: string, failed: string, canceled: string, error: string, copy: string, edit: string, reasoning: string, thought: string};

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
  addDelegatedEventListener(form, 'click', '.agent-chat-attach-more', expandAttachments);
  return clear;
}

// ChatGPT and Claude send on Enter and grow the input while typing; a chat box that stays
// three rows tall wastes half the panel.
function initComposerKeys(form: HTMLFormElement, input: HTMLTextAreaElement) {
  input.addEventListener('keydown', (event) => {
    if (event.key !== 'Enter' || event.shiftKey || event.isComposing) return;
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
function initDraftComposer() {
  const form = document.querySelector<HTMLFormElement>('.agent-chat-composer[data-draft-url]');
  if (!form) return;
  const button = form.querySelector<HTMLButtonElement>('button[type="submit"]')!;
  initAttachmentList(form);

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

const bottomThreshold = 80; // px

// the panel fills the space between the navbar and the footer, both of which depend on
// the viewport and the locale, so the height is measured instead of guessed in CSS
function fitLayout() {
  const chat = document.querySelector<HTMLElement>('.agent-chat');
  const footer = document.querySelector<HTMLElement>('footer');
  if (!chat || !footer) return;
  const top = chat.getBoundingClientRect().top + window.scrollY;
  // the page owns the viewport, so the footer sits at its bottom and the panel fills the rest
  const height = `${Math.max(240, Math.round(innerHeight - top - footer.offsetHeight - 8))}px`;
  if (chat.style.height !== height) chat.style.height = height; // writing the same value loops the observer
}

export function initAgentChat() {
  fitLayout();
  // the viewport and the footer both drive the panel height
  const observer = new ResizeObserver(fitLayout);
  observer.observe(document.documentElement);
  const footer = document.querySelector<HTMLElement>('footer');
  if (footer) observer.observe(footer);
  initDraftComposer();
  initMoreMenus();

  // the session list marks the conversations that are answering right now
  const chatEl = document.querySelector<HTMLElement>('.agent-chat');
  const sessionBusyLabel = chatEl?.getAttribute('data-busy-label') ?? '';

  // `data-conversation-id` marks the page as a live chat, all URLs and labels are rendered by the server
  const mainEl = document.querySelector<HTMLElement>('.agent-chat-main[data-conversation-id]');
  if (!mainEl) {
    // a page without an open conversation still follows the runs of the reader's sessions
    const runsUrl = chatEl?.getAttribute('data-runs-url');
    if (runsUrl && document.querySelector('.agent-chat-card[data-session-id]')) {
      followAgentLive(runsUrl, {
        onRun: (run) => markSessionBusy(String(run.conversationId), runIsBusy(run), sessionBusyLabel),
      });
    }
    return;
  }

  const eventsUrl = mainEl.getAttribute('data-events-url')!;
  const messagesUrl = mainEl.getAttribute('data-messages-url')!;
  const cancelUrl = mainEl.getAttribute('data-cancel-url')!;
  const labels = JSON.parse(mainEl.getAttribute('data-labels')!) as AgentChatLabels;
  const moreLabel = mainEl.closest<HTMLElement>('.agent-chat')?.getAttribute('data-attach-more-label') ?? '';
  const approvalsUrl = mainEl.getAttribute('data-approvals-url')!;
  const approvalLabels = {
    title: mainEl.getAttribute('data-approval-title')!, // the tool name is interpolated where the locale keeps %s
    help: mainEl.getAttribute('data-approval-help')!,
    allow: mainEl.getAttribute('data-approval-allow')!,
    refuse: mainEl.getAttribute('data-approval-refuse')!,
  };
  const messagesEl = mainEl.querySelector<HTMLElement>('.agent-chat-messages')!;
  const composerEl = mainEl.querySelector<HTMLFormElement>('.agent-chat-composer')!;
  const sendEl = mainEl.querySelector<HTMLButtonElement>('.agent-chat-send')!;
  const noteEl = mainEl.querySelector<HTMLElement>('.agent-chat-composer-error')!;
  const inputEl = mainEl.querySelector<HTMLTextAreaElement>('.agent-chat-input')!;
  const ajaxHeaders = {'X-Requested-With': 'XMLHttpRequest'};
  const elConversationID = mainEl.getAttribute('data-conversation-id')!;
  const editURL = `${mainEl.getAttribute('data-messages-url')}/`;
  // (editURL keeps the trailing slash: the per-message URL appends the message id)
  const idleLabel = sendEl.getAttribute('data-idle-label')!;
  const busyLabel = sendEl.getAttribute('data-busy-label')!;
  let busy = sendEl.hasAttribute('data-busy');

  // without EventSource nothing is streamed, so the composer keeps its native submit (redirect)
  if (!window.EventSource) return;

  // the send button reports a running answer: it turns into the cancel button
  function setBusy(next: boolean, note = '') {
    busy = next;
    sendEl.textContent = busy ? busyLabel : idleLabel;
    sendEl.classList.toggle('busy', busy);
    noteEl.textContent = note;
    toggleElem(noteEl, note !== '');
  }

  // a call and a thought fold the same way: one faint line, and what it holds behind it
  function buildFold(summary: Node[], body: string): HTMLDetailsElement {
    const detailsEl = document.createElement('details');
    detailsEl.className = 'agent-chat-fold';
    const summaryEl = document.createElement('summary');
    summaryEl.className = 'agent-chat-fold-summary';
    summaryEl.insertAdjacentHTML('beforeend', svg('octicon-chevron-right', 12, 'agent-chat-fold-chevron'));
    summaryEl.append(...summary);
    const bodyEl = document.createElement('div');
    bodyEl.className = 'agent-chat-fold-body';
    const preEl = document.createElement('pre');
    preEl.textContent = body;
    bodyEl.append(preEl);
    detailsEl.append(summaryEl, bodyEl);
    return detailsEl;
  }

  function buildMessage(message: AgentMessage): HTMLElement {
    const messageEl = document.createElement('div');
    messageEl.className = `agent-chat-message ${message.role}`;
    messageEl.setAttribute('data-message-id', String(message.id));

    const metaEl = document.createElement('div');
    metaEl.className = 'agent-chat-message-meta';
    metaEl.textContent = `${message.roleLabel} · `;
    metaEl.insertAdjacentHTML('beforeend', message.timeHTML);

    // a tool record folds into one faint line: the call, and what came back behind it
    if (message.role === 'tool') {
      messageEl.classList.add('agent-chat-tool');
      const nameEl = document.createElement('code');
      nameEl.className = 'agent-chat-tool-name';
      nameEl.textContent = message.tool?.name ?? '';
      const summary: Node[] = [nameEl];
      if (message.tool?.args) {
        const argsEl = document.createElement('span');
        argsEl.className = 'agent-chat-tool-args gt-ellipsis';
        argsEl.textContent = message.tool.args;
        summary.push(argsEl);
      }
      const timeEl = document.createElement('span');
      timeEl.className = 'agent-chat-message-meta';
      timeEl.innerHTML = message.timeHTML;
      summary.push(timeEl);
      messageEl.append(buildFold(summary, message.content));
      return messageEl;
    }

    messageEl.append(metaEl);
    // what the model thought before the answer folds away above it
    if (message.reasoning) {
      const labelEl = document.createElement('span');
      labelEl.textContent = labels.thought;
      const thoughtEl = buildFold([labelEl], message.reasoning);
      thoughtEl.classList.add('agent-chat-thought');
      messageEl.append(thoughtEl);
    }

    const bubbleEl = document.createElement('div');
    bubbleEl.className = 'agent-chat-bubble';
    const renderedEl = document.createElement('div');
    renderedEl.className = 'render-content markup';
    renderedEl.innerHTML = message.html;
    bubbleEl.append(renderedEl);

    if (message.attachments && message.attachments.length > 0) {
      const listEl = document.createElement('div');
      listEl.className = 'agent-chat-attachments';
      const picturesEl = document.createElement('div');
      picturesEl.className = 'agent-chat-attach-group agent-chat-attach-pictures';
      const filesEl = document.createElement('div');
      filesEl.className = 'agent-chat-attach-group agent-chat-attach-files';
      for (const attachment of message.attachments) {
        if (attachment.isPicture) {
          const imageEl = document.createElement('a');
          imageEl.className = 'agent-chat-attachment-image';
          imageEl.setAttribute('href', attachment.url);
          imageEl.setAttribute('target', '_blank');
          imageEl.setAttribute('rel', 'noreferrer noopener');
          const image = document.createElement('img');
          image.setAttribute('src', attachment.url);
          image.setAttribute('alt', attachment.name);
          image.setAttribute('loading', 'lazy');
          imageEl.append(image);
          picturesEl.append(imageEl);
          continue;
        }
        const linkEl = document.createElement('a');
        linkEl.className = 'agent-chat-attachment';
        linkEl.setAttribute('href', attachment.url);
        linkEl.setAttribute('download', '');
        const nameEl = document.createElement('span');
        nameEl.className = 'gt-ellipsis';
        nameEl.textContent = attachment.name;
        const sizeEl = document.createElement('span');
        sizeEl.className = 'agent-chat-attachment-size';
        sizeEl.textContent = attachment.sizeText;
        linkEl.append(createElementFromHTML(svg('octicon-paperclip', 14)), nameEl, sizeEl);
        filesEl.append(linkEl);
      }
      limitAttachmentGroup(picturesEl, moreLabel, true);
      limitAttachmentGroup(filesEl, moreLabel);
      const groups = [picturesEl, filesEl].filter((group) => group.childElementCount > 0);
      listEl.append(...groups);
      messageEl.append(listEl); // files travel under the bubble, not inside it
    }

    const actionsEl = document.createElement('div');
    actionsEl.className = 'agent-chat-message-actions';
    actionsEl.insertAdjacentHTML('beforeend', `<button type="button" class="agent-chat-copy-message" data-tooltip-content="${labels.copy}">${svg('octicon-copy', 14)}</button>`);
    if (message.role === 'user') {
      actionsEl.insertAdjacentHTML('beforeend', `<button type="button" class="agent-chat-edit-message show-modal" data-modal="#agent-chat-edit-modal" data-modal-form.url="${editURL}${message.id}" data-tooltip-content="${labels.edit}">${svg('octicon-pencil', 14)}</button>`);
    }

    const sourceEl = document.createElement('div');
    sourceEl.className = 'agent-chat-source tw-hidden';
    sourceEl.textContent = message.content;

    messageEl.append(bubbleEl, actionsEl, sourceEl);
    return messageEl;
  }

  // the run of this session: approvals and the tool records they answer belong to it
  let currentRunID = Number(mainEl.getAttribute('data-run-id') ?? 0);

  function appendMessage(message: AgentMessage) {
    if (messagesEl.querySelector(`[data-message-id="${CSS.escape(String(message.id))}"]`)) return;
    dropPending();
    // the answer to a tool call has arrived, so its card has served its purpose
    if (message.role === 'tool') dropApprovals(currentRunID);

    for (const el of messagesEl.querySelectorAll('.agent-chat-empty')) el.remove();

    messagesEl.append(buildMessage(message));
    mainEl!.setAttribute('data-last-message-id', String(message.id));
    if (message.role === 'user') {
      // the browser keeps the caret in view, so wait for the input to settle before anchoring
      requestAnimationFrame(() => {
        const users = messagesEl.querySelectorAll<HTMLElement>('.agent-chat-message.user');
        const bubble = users[users.length - 1]?.querySelector<HTMLElement>('.agent-chat-bubble') ?? users[users.length - 1];
        // the chat input steals focus first: without it the browser pulls the caret back into view
        scrollToTop(bubble, () => {
          composerEl.querySelector<HTMLTextAreaElement>('.agent-chat-input')?.focus({preventScroll: true});
        });
      });
      return;
    }
    // the question stays pinned until the reader moves; once they scroll to read, follow normally
    const follow = messagesEl.scrollTop >= messagesEl.scrollHeight - messagesEl.clientHeight - bottomThreshold;
    if (follow) messagesEl.scrollTop = messagesEl.scrollHeight;
  }

  // the transcript loads its tail first; older messages arrive on demand
  async function loadOlder() {
    const button = messagesEl.querySelector<HTMLElement>('.agent-chat-load-older');
    if (!button || button.classList.contains('disabled')) return;
    button.classList.add('disabled', 'is-loading');
    const url = `${button.getAttribute('data-messages-url')}?before=${button.getAttribute('data-oldest')}`;
    try {
      const resp = await GET(url);
      const older = await resp.json() as AgentMessage[];
      const anchor = messagesEl.scrollHeight - messagesEl.scrollTop;
      const first = messagesEl.querySelector('.agent-chat-message');
      for (const message of older) {
        messagesEl.insertBefore(buildMessage(message), first);
      }
      if (older.length > 0) {
        button.setAttribute('data-oldest', String(older[0].id));
        mainEl!.setAttribute('data-oldest-message-id', String(older[0].id));
      }
      if (older.length < 50) button.remove();
      else button.classList.remove('disabled', 'is-loading');
      for (const image of messagesEl.querySelectorAll('.agent-chat-attachment-image img[loading="lazy"]')) {
        image.removeAttribute('loading');
      }
      messagesEl.scrollTop = messagesEl.scrollHeight - anchor; // keep the reading position
    } catch {
      button.classList.remove('disabled', 'is-loading');
    }
  }

  // the reader cares about the answer to their last message: show the question and the
  // answer block together when they fit, the newest content when they do not
  function scrollToLastQuestion() {
    const users = messagesEl.querySelectorAll<HTMLElement>('.agent-chat-message.user');
    const last = users[users.length - 1];
    if (!last) {
      messagesEl.scrollTop = messagesEl.scrollHeight;
      return;
    }
    const bubble = last.querySelector<HTMLElement>('.agent-chat-bubble') ?? last;
    const pending = messagesEl.querySelector<HTMLElement>('.agent-chat-pending');
    if (pending && (pending.offsetTop + pending.offsetHeight - bubble.offsetTop) <= messagesEl.clientHeight) {
      scrollToTop(bubble); // question and answer fit: the question opens the view
      return;
    }
    messagesEl.scrollTop = messagesEl.scrollHeight; // they do not fit: follow the newest
  }

  // a scroll that lands before the next layout pass gets pulled back, so settle first
  function scrollToTop(target: HTMLElement, after?: () => void) {
    // offsetTop is document-relative: subtract the container, then clamp to its scroll range
    const max = messagesEl.scrollHeight - messagesEl.clientHeight;
    const wanted = Math.min(max, Math.max(0, target.offsetTop - messagesEl.offsetTop - 4));
    requestAnimationFrame(() => {
      messagesEl.scrollTop = wanted;
      after?.();
      requestAnimationFrame(() => {
        if (messagesEl.scrollTop !== wanted) messagesEl.scrollTop = wanted;
      });
    });
  }

  function initTranscript() {
    addDelegatedEventListener(messagesEl, 'click', '.agent-chat-copy-message', (button) => {
      const source = button.closest('.agent-chat-message')!.querySelector('.agent-chat-source');
      copyToClipboardWithFeedback(button, source!.textContent);
    });
    messagesEl.querySelector('.agent-chat-load-older')?.addEventListener('click', loadOlder);
    addDelegatedEventListener(messagesEl, 'click', '.agent-chat-attach-more', expandAttachments);
  }

  // the block under the last message says what its answer is doing; the stored message
  // replaces it once the run succeeds
  type AgentChatPendingState = 'queued' | 'running' | 'reasoning' | 'streaming' | 'failed' | 'canceled';

  let pendingEl = messagesEl.querySelector<HTMLElement>('.agent-chat-pending');
  let pendingRunID = Number(pendingEl?.getAttribute('data-run-id') ?? 0);

  function dropPending() {
    pendingEl?.remove();
    pendingEl = null;
  }

  function ensurePending(): HTMLElement {
    if (pendingEl) return pendingEl;
    for (const el of messagesEl.querySelectorAll('.agent-chat-empty')) el.remove();
    const el = document.createElement('div');
    el.className = 'agent-chat-message assistant agent-chat-pending';
    const labelEl = document.createElement('span');
    labelEl.textContent = labels.thought;
    const thoughtEl = buildFold([labelEl], '');
    thoughtEl.classList.add('agent-chat-thought', 'tw-hidden'); // no fold until there is a thought
    const bubbleEl = document.createElement('div');
    bubbleEl.className = 'agent-chat-bubble';
    bubbleEl.innerHTML = `<div class="agent-chat-pending-status"><span class="agent-chat-reasoning-dots"><i></i><i></i><i></i></span><span class="agent-chat-pending-label"></span></div>
      <div class="agent-chat-live-text"></div>`;
    el.append(thoughtEl, bubbleEl);
    messagesEl.append(el);
    pendingEl = el;
    return el;
  }

  function setPendingState(state: AgentChatPendingState, text: string) {
    const el = ensurePending();
    el.setAttribute('data-state', state);
    el.querySelector('.agent-chat-pending-label')!.textContent = text;
    // once text arrives the label would only repeat what is already readable
    toggleElem(el.querySelector<HTMLElement>('.agent-chat-pending-status')!, state !== 'streaming');
  }

  function applyDelta(delta: AgentDelta) {
    if (delta.reset) {
      dropPending();
      pendingRunID = 0;
      return; // another role member starts over
    }
    if (!delta.content && !delta.reasoning) return;
    if (delta.runId !== pendingRunID) {
      dropPending();
      pendingRunID = delta.runId;
    }

    const atBottom = messagesEl.scrollHeight - messagesEl.scrollTop - messagesEl.clientHeight <= bottomThreshold;
    if (delta.reasoning) {
      const thoughtEl = ensurePending().querySelector<HTMLElement>('.agent-chat-thought')!;
      toggleElem(thoughtEl, true);
      thoughtEl.querySelector('pre')!.textContent += delta.reasoning;
    }
    if (delta.content) {
      ensurePending().querySelector<HTMLElement>('.agent-chat-live-text')!.textContent += delta.content;
      setPendingState('streaming', '');
    } else {
      setPendingState('reasoning', labels.reasoning); // the model thinks before it answers
    }
    // follow the answer only when the reader was already at the bottom
    if (atBottom) messagesEl.scrollTop = messagesEl.scrollHeight;
  }

  // a tool call the pod is waiting for: the card posts the decision, so the page reloads
  function buildApproval(approval: AgentApproval): HTMLElement {
    const el = document.createElement('div');
    el.className = 'agent-chat-approval';
    el.setAttribute('data-approval-id', String(approval.id));
    el.setAttribute('data-run-id', String(approval.runID));

    const titleEl = document.createElement('div');
    titleEl.className = 'agent-chat-approval-title';
    const [titlePrefix, titleSuffix = ''] = approvalLabels.title.split('%s');
    titleEl.textContent = titlePrefix + approval.tool + titleSuffix;

    const toolEl = document.createElement('div');
    toolEl.className = 'agent-chat-approval-tool';
    const nameEl = document.createElement('span');
    nameEl.className = 'agent-chat-approval-name';
    nameEl.textContent = approval.tool;
    toolEl.append(nameEl);
    if (approval.arguments) {
      const argsEl = document.createElement('span');
      argsEl.className = 'agent-chat-approval-args';
      argsEl.textContent = approval.arguments;
      toolEl.append(argsEl);
    }

    const helpEl = document.createElement('p');
    helpEl.className = 'agent-chat-approval-help';
    helpEl.textContent = approvalLabels.help;

    const actionsEl = document.createElement('div');
    actionsEl.className = 'agent-chat-approval-actions';
    for (const [action, label, buttonClass] of [
      ['allow', approvalLabels.allow, 'ui small primary button'],
      ['refuse', approvalLabels.refuse, 'ui small button'],
    ]) {
      const formEl = document.createElement('form');
      formEl.method = 'post';
      formEl.action = `${approvalsUrl}/${approval.id}/${action}`;
      const buttonEl = document.createElement('button');
      buttonEl.type = 'submit';
      buttonEl.className = buttonClass;
      buttonEl.textContent = label;
      formEl.append(buttonEl);
      actionsEl.append(formEl);
    }

    el.append(titleEl, toolEl, helpEl, actionsEl);
    return el;
  }

  // the engine stopped waiting: the cards of that run are stale, whatever the reader did
  function dropApprovals(runID: number) {
    for (const el of messagesEl.querySelectorAll<HTMLElement>('.agent-chat-approval')) {
      if (!runID || Number(el.getAttribute('data-run-id')) === runID) el.remove();
    }
  }

  function appendApproval(approval: AgentApproval) {
    if (messagesEl.querySelector(`[data-approval-id="${CSS.escape(String(approval.id))}"]`)) return;
    const atBottom = messagesEl.scrollHeight - messagesEl.scrollTop - messagesEl.clientHeight <= bottomThreshold;
    for (const el of messagesEl.querySelectorAll('.agent-chat-empty')) el.remove();
    messagesEl.append(buildApproval(approval));
    // the reader stays where they left the transcript
    if (atBottom) messagesEl.scrollTop = messagesEl.scrollHeight;
  }

  function applyRun(run: AgentRun) {
    currentRunID = run.id;
    markSessionBusy(elConversationID, runIsBusy(run), sessionBusyLabel);
    if (run.status === 'queued' || run.status === 'running') {
      setBusy(true);
      const state = pendingEl?.getAttribute('data-state');
      if (run.status === 'queued' || !state || state === 'queued') {
        setPendingState(run.status === 'queued' ? 'queued' : 'running', run.status === 'queued' ? labels.queued : labels.running);
      }
      return;
    }
    setBusy(false);
    dropApprovals(run.id);
    if (run.status === 'succeeded') {
      dropPending(); // the stored message takes its place
      pendingRunID = 0;
      return;
    }
    // the failure stays under the message it belongs to
    setPendingState(run.status === 'failed' ? 'failed' : 'canceled',
      run.status === 'failed' ? (run.error ? `${labels.failed}: ${run.error}` : labels.failed) : labels.canceled);
    pendingRunID = 0;
  }

  // the folder modals are shared by every row: the global "show-modal" handler fills the
  // form action, the icon radio and the folder select are set here
  // the edit modal is shared: it takes the text of the clicked message
  function initMessageActions() {
    const editForm = document.querySelector<HTMLFormElement>('#agent-chat-edit-form');
    if (!editForm) return;
    addDelegatedEventListener(messagesEl, 'click', '.agent-chat-edit-message', (button) => {
      const source = button.closest('.agent-chat-message')!.querySelector('.agent-chat-source');
      editForm.setAttribute('action', button.getAttribute('data-modal-form.url')!);
      editForm.querySelector<HTMLTextAreaElement>('textarea[name="content"]')!.value = source!.textContent!;
    });
  }

  function initMoreMenus() {
    // the menu is a popover, so the scrolling list cannot clip it
    for (const toggle of document.querySelectorAll<HTMLElement>('.agent-chat-more')) {
      const menu = toggle.parentElement!.querySelector<HTMLElement>('.agent-chat-more-menu');
      if (!menu) continue;
      toggle.addEventListener('click', () => {
        const box = toggle.getBoundingClientRect();
        menu.style.top = `${box.bottom + 4}px`;
        menu.style.right = `${innerWidth - box.right}px`;
        menu.showPopover();
      });
      // an item opens a modal, so the menu must not stay behind it
      menu.addEventListener('click', (event) => {
        if ((event.target as HTMLElement).closest('.agent-chat-more-item')) menu.hidePopover();
      });
    }
    // a fixed menu would drift away from its row while the list scrolls
    document.addEventListener('scroll', () => {
      for (const menu of document.querySelectorAll<HTMLElement>('.agent-chat-more-menu:popover-open')) {
        menu.hidePopover();
      }
    }, {capture: true});

    const folderModal = document.querySelector<HTMLFormElement>('#agent-chat-folder-form');
    const pickModal = document.querySelector<HTMLFormElement>('#agent-chat-folder-pick-form');
    for (const item of document.querySelectorAll<HTMLElement>('.agent-chat-more-item[data-icon]')) {
      item.addEventListener('click', () => {
        const icon = item.getAttribute('data-icon')!;
        for (const input of folderModal!.querySelectorAll<HTMLInputElement>('input[name="icon"]')) {
          input.checked = input.value === icon;
        }
      }, {capture: true}); // run before the global handler opens the modal
    }
    for (const item of document.querySelectorAll<HTMLElement>('.agent-chat-more-item[data-folder]')) {
      item.addEventListener('click', () => {
        const folder = item.getAttribute('data-folder')!;
        pickModal!.querySelector<HTMLSelectElement>('select[name="folder_id"]')!.value = folder;
      }, {capture: true});
    }
  }

  // a renamed session shows its new title in the list and in the chat header
  function applyConversation(conversation: AgentConversation) {
    const header = document.querySelector<HTMLElement>('.agent-chat-head-title');
    if (header && elConversationID === String(conversation.id)) header.textContent = conversation.title;
    const id = String(conversation.id);
    for (const link of document.querySelectorAll<HTMLAnchorElement>('.agent-chat-card-main, a.agent-chat-card')) {
      const href = link.getAttribute('href');
      if (!href || href.split('/').pop() !== id) continue;
      const label = link.querySelector<HTMLElement>('.gt-ellipsis, .agent-chat-card-name');
      // the resident session shows what it worked on instead of its own name
      if (label) label.textContent = conversation.summary || conversation.title;
    }
  }

  // the stream carries this conversation and the run changes of the reader's other sessions,
  // so the list beside the transcript stays live; the cursor it reopens with brings back
  // whatever arrived while the page was hidden
  followAgentLive(() => `${eventsUrl}?since=${mainEl.getAttribute('data-last-message-id')}`, {
    onMessage: (message) => appendMessage(message),
    onApproval: (approval) => appendApproval(approval),
    onDelta: (delta) => applyDelta(delta),
    onConversation: (conversation) => applyConversation(conversation),
    onRun: (run) => {
      if (String(run.conversationId) === elConversationID) {
        applyRun(run);
      } else {
        markSessionBusy(String(run.conversationId), runIsBusy(run), sessionBusyLabel);
      }
    },
  });

  initTranscript();
  initMessageActions();
  scrollToLastQuestion();
  // older messages can carry pictures; ask for them now, before anything scrolls
  for (const image of messagesEl.querySelectorAll('.agent-chat-attachment-image img[loading="lazy"]')) {
    image.removeAttribute('loading');
  }
  const clearAttachments = initAttachmentList(composerEl);
  initComposerKeys(composerEl, inputEl);

  composerEl.addEventListener('submit', async (event) => {
    event.preventDefault();
    if (busy) {
      POST(cancelUrl, {headers: ajaxHeaders}); // the same button stops the answer
      return;
    }
    try {
      const resp = await POST(messagesUrl, {data: new FormData(composerEl), headers: ajaxHeaders});
      if (!resp.ok) {
        setBusy(false, `${labels.error} (${resp.status} ${resp.statusText})`);
        return;
      }
      inputEl.value = '';
      clearAttachments(); // the files are sent, so the next message starts without them
      if (!pendingEl) setPendingState('queued', labels.queued); // the run event follows, this avoids a gap
    } catch (error) {
      setBusy(false, `${labels.error} (${errorMessage(error)})`);
    }
  });
}
