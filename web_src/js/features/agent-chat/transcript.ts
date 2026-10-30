import {copyToClipboardWithFeedback} from '../../modules/clipboard.ts';
import {svg} from '../../svg.ts';
import {addDelegatedEventListener, createElementFromHTML, toggleElem} from '../../utils/dom.ts';
import type {AgentApproval, AgentMessage} from '../../modules/agent-live.ts';

export type AgentChatLabels = {queued: string, running: string, failed: string, canceled: string, error: string, copy: string, edit: string, reasoning: string, thought: string};

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

// a call and a thought fold the same way: one faint line, and what it holds behind it
export function buildFold(summary: Node[], body: string): HTMLDetailsElement {
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

export function buildMessage(message: AgentMessage, opts: {
  labels: AgentChatLabels,
  moreLabel: string,
  editURL: string,
  names: Map<string, string>,
}): HTMLElement {
  const {labels, moreLabel, editURL, names} = opts;
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
  decorateTaskTokens(bubbleEl, names); // the words read as the names they stand for
  return messageEl;
}

// A message carries the words its writer picked ("/issue-triage", "@acme/widgets#2"). The
// transcript reads them the way the composer did — as the names they stand for, marked like
// every other token — so what was sent reads like what was written.
const taskWordPattern = /(^|\s)([/@][a-z0-9][a-z0-9_./#-]*)/g;

export function decorateTaskTokens(root: HTMLElement, names: Map<string, string>) {
  // the words inside a link or a code block are not tasks, and the copy the reader takes has
  // to be the message exactly as it was sent
  const walker = document.createTreeWalker(root, NodeFilter.SHOW_TEXT, {
    acceptNode: (node) => node.parentElement?.closest('a, code, pre, .agent-chat-token, .agent-chat-source') === null ?
      NodeFilter.FILTER_ACCEPT :
      NodeFilter.FILTER_REJECT,
  });
  const texts: Text[] = [];
  while (walker.nextNode()) texts.push(walker.currentNode as Text);

  for (const node of texts) {
    const text = node.data;
    let at = 0;
    const parts: Node[] = [];
    for (const match of text.matchAll(taskWordPattern)) {
      const word = match[2];
      const start = (match.index ?? 0) + match[1].length;
      if (start > at) parts.push(document.createTextNode(text.slice(at, start)));
      const el = document.createElement('span');
      el.className = `agent-chat-token agent-chat-token-${word.startsWith('/') ? 'command' : 'subject'}`;
      el.textContent = names.get(word) ?? word;
      parts.push(el);
      at = start + word.length;
    }
    if (parts.length === 0) continue;
    if (at < text.length) parts.push(document.createTextNode(text.slice(at)));
    node.replaceWith(...parts);
  }
}

// the run waits for the reader: the card posts the decision, so the transcript reloads
export function buildApproval(approval: AgentApproval, approvalLabels: {
  title: string, help: string, allow: string, refuse: string,
}, approvalsUrl: string): HTMLElement {
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

// the reader cares about the answer to their last message: show the question and the
// answer block together when they fit, the newest content when they do not
export function scrollToLastQuestion(messagesEl: HTMLElement) {
  const users = messagesEl.querySelectorAll<HTMLElement>('.agent-chat-message.user');
  const last = users[users.length - 1];
  if (!last) {
    messagesEl.scrollTop = messagesEl.scrollHeight;
    return;
  }
  const bubble = last.querySelector<HTMLElement>('.agent-chat-bubble') ?? last;
  const pending = messagesEl.querySelector<HTMLElement>('.agent-chat-pending');
  if (pending && (pending.offsetTop + pending.offsetHeight - bubble.offsetTop) <= messagesEl.clientHeight) {
    messagesEl.scrollTop = Math.min(messagesEl.scrollHeight - messagesEl.clientHeight, Math.max(0, bubble.offsetTop - messagesEl.offsetTop - 4));
    return;
  }
  messagesEl.scrollTop = messagesEl.scrollHeight; // they do not fit: follow the newest
}

// a scroll that lands before the next layout pass gets pulled back, so settle first
export function scrollToTop(messagesEl: HTMLElement, target: HTMLElement, after?: () => void) {
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

// copies, the older-messages button and the row reveal are transcript-wide listeners
export function initTranscriptActions(messagesEl: HTMLElement) {
  addDelegatedEventListener(messagesEl, 'click', '.agent-chat-copy-message', (button) => {
    const source = button.closest('.agent-chat-message')!.querySelector('.agent-chat-source');
    copyToClipboardWithFeedback(button, source!.textContent);
  });
  addDelegatedEventListener(messagesEl, 'click', '.agent-chat-attach-more', (button) => {
    for (const item of button.parentElement?.querySelectorAll<HTMLElement>('.tw-hidden') ?? []) toggleElem(item, true);
    button.remove();
  });
}

// the edit modal is shared: it takes the text of the clicked message
export function initMessageActions(messagesEl: HTMLElement) {
  const editForm = document.querySelector<HTMLFormElement>('#agent-chat-edit-form');
  if (!editForm) return;
  addDelegatedEventListener(messagesEl, 'click', '.agent-chat-edit-message', (button) => {
    const source = button.closest('.agent-chat-message')!.querySelector('.agent-chat-source');
    editForm.setAttribute('action', button.getAttribute('data-modal-form.url')!);
    editForm.querySelector<HTMLTextAreaElement>('textarea[name="content"]')!.value = source!.textContent!;
  });
}
