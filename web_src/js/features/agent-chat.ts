import {GET, POST} from '../modules/fetch.ts';
import {copyToClipboardWithFeedback} from '../modules/clipboard.ts';
import {svg} from '../svg.ts';
import {errorMessage} from '../modules/errors.ts';
import {addDelegatedEventListener, createElementFromHTML, hideElem, showElem, toggleElem} from '../utils/dom.ts';

type AgentChatLabels = {queued: string, running: string, failed: string, canceled: string, cancel: string, error: string, copy: string, edit: string};
type AgentChatAttachment = {id: number, name: string, sizeText: string, url: string};
type AgentChatMessage = {id: number, role: string, roleLabel: string, content: string, html: string, timeHTML: string, attachments?: AgentChatAttachment[]};
type AgentChatRun = {id: number, status: string, error: string, cancelable: boolean};
type AgentChatConversation = {id: number, title: string, summary: string, icon: string};

// a conversation does not exist until its first message, so the draft posts the message and
// then follows the created conversation
function initDraftComposer() {
  const form = document.querySelector<HTMLFormElement>('.agent-chat-composer[data-draft-url]');
  if (!form) return;
  const button = form.querySelector<HTMLButtonElement>('button[type="submit"]')!;

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

const reconnectDelay = 2000; // ms, the browser's own reconnect would replay the cursor from the first connect
const bottomThreshold = 80; // px
const messageTopMargin = 8; // px above the last question when the transcript opens

export function initAgentChat() {
  initDraftComposer();
  initMoreMenus();

  // `data-conversation-id` marks the page as a live chat, all URLs and labels are rendered by the server
  const mainEl = document.querySelector<HTMLElement>('.agent-chat-main[data-conversation-id]');
  if (!mainEl) return;

  const eventsUrl = mainEl.getAttribute('data-events-url')!;
  const messagesUrl = mainEl.getAttribute('data-messages-url')!;
  const cancelUrl = mainEl.getAttribute('data-cancel-url')!;
  const labels = JSON.parse(mainEl.getAttribute('data-labels')!) as AgentChatLabels;
  const messagesEl = mainEl.querySelector<HTMLElement>('.agent-chat-messages')!;
  const statusEl = mainEl.querySelector<HTMLElement>('.agent-chat-status')!;
  const statusTextEl = mainEl.querySelector<HTMLElement>('.agent-chat-status-text')!;
  const statusCancelEl = mainEl.querySelector<HTMLElement>('.agent-chat-status-cancel')!;
  const composerEl = mainEl.querySelector<HTMLFormElement>('.agent-chat-composer')!;
  const inputEl = mainEl.querySelector<HTMLTextAreaElement>('.agent-chat-input')!;
  const ajaxHeaders = {'X-Requested-With': 'XMLHttpRequest'};
  const elConversationID = mainEl.getAttribute('data-conversation-id')!;
  const editURL = `${mainEl.getAttribute('data-messages-url')}/`;

  statusCancelEl.textContent = labels.cancel;
  statusCancelEl.addEventListener('click', () => {
    POST(cancelUrl, {headers: ajaxHeaders}); // no await
  });

  // without EventSource nothing is streamed, so the composer keeps its native submit (redirect)
  if (!window.EventSource) return;

  function showStatus(status: string, text: string, cancelable: boolean) {
    // the CSS colors the strip by `data-status`, the server set it to the last known run
    statusEl.setAttribute('data-status', status);
    statusTextEl.textContent = text;
    toggleElem(statusCancelEl, cancelable);
    showElem(statusEl);
  }

  function buildMessage(message: AgentChatMessage): HTMLElement {
    const messageEl = document.createElement('div');
    messageEl.className = `agent-chat-message ${message.role}`;
    messageEl.setAttribute('data-message-id', String(message.id));

    const metaEl = document.createElement('div');
    metaEl.className = 'agent-chat-message-meta';
    metaEl.textContent = `${message.roleLabel} · `;
    metaEl.insertAdjacentHTML('beforeend', message.timeHTML);

    const bubbleEl = document.createElement('div');
    bubbleEl.className = 'agent-chat-bubble';
    const renderedEl = document.createElement('div');
    renderedEl.className = 'render-content markup';
    renderedEl.innerHTML = message.html;
    bubbleEl.append(renderedEl);

    if (message.attachments && message.attachments.length > 0) {
      const listEl = document.createElement('div');
      listEl.className = 'agent-chat-attachments';
      for (const attachment of message.attachments) {
        const linkEl = document.createElement('a');
        linkEl.className = 'agent-chat-attachment';
        linkEl.setAttribute('href', attachment.url);
        const nameEl = document.createElement('span');
        nameEl.className = 'gt-ellipsis';
        nameEl.textContent = attachment.name;
        const sizeEl = document.createElement('span');
        sizeEl.className = 'agent-chat-attachment-size';
        sizeEl.textContent = attachment.sizeText;
        linkEl.append(createElementFromHTML(svg('octicon-paperclip', 14)), nameEl, sizeEl);
        listEl.append(linkEl);
      }
      bubbleEl.append(listEl);
    }

    const actionsEl = document.createElement('div');
    actionsEl.className = 'agent-chat-message-actions';
    actionsEl.insertAdjacentHTML('beforeend', `<button type="button" class="agent-chat-copy-message" data-tooltip-content="${labels.copy}">${svg('octicon-copy', 14)}</button>`);
    if (message.role === 'user') {
      actionsEl.insertAdjacentHTML('beforeend', `<button type="button" class="agent-chat-edit-message show-modal" data-modal="#agent-chat-edit-modal" data-modal-form.url="${editURL}" data-tooltip-content="${labels.edit}">${svg('octicon-pencil', 14)}</button>`);
    }

    const sourceEl = document.createElement('div');
    sourceEl.className = 'agent-chat-source tw-hidden';
    sourceEl.textContent = message.content;

    messageEl.append(metaEl, bubbleEl, actionsEl, sourceEl);
    return messageEl;
  }

  function appendMessage(message: AgentChatMessage) {
    if (messagesEl.querySelector(`[data-message-id="${CSS.escape(String(message.id))}"]`)) return;

    const atBottom = messagesEl.scrollHeight - messagesEl.scrollTop - messagesEl.clientHeight <= bottomThreshold;
    for (const el of messagesEl.querySelectorAll('.agent-chat-empty')) el.remove();

    messagesEl.append(buildMessage(message));
    mainEl!.setAttribute('data-last-message-id', String(message.id));
    if (atBottom) messagesEl.scrollTop = messagesEl.scrollHeight;
  }

  // the transcript loads its tail first; older messages arrive on demand
  async function loadOlder() {
    const button = messagesEl.querySelector<HTMLElement>('.agent-chat-load-older');
    if (!button || button.classList.contains('disabled')) return;
    button.classList.add('disabled', 'is-loading');
    const url = `${button.getAttribute('data-messages-url')}?before=${button.getAttribute('data-oldest')}`;
    try {
      const resp = await GET(url);
      const older = await resp.json() as AgentChatMessage[];
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
      messagesEl.scrollTop = messagesEl.scrollHeight - anchor; // keep the reading position
    } catch {
      button.classList.remove('disabled', 'is-loading');
    }
  }

  // the reader cares about the answer to their last message, so that is where the view starts
  function scrollToLastQuestion() {
    const users = messagesEl.querySelectorAll<HTMLElement>('.agent-chat-message.user');
    const last = users[users.length - 1];
    if (!last) {
      messagesEl.scrollTop = messagesEl.scrollHeight;
      return;
    }
    const delta = last.getBoundingClientRect().top - messagesEl.getBoundingClientRect().top;
    messagesEl.scrollTop += delta - messageTopMargin;
  }

  function initTranscript() {
    addDelegatedEventListener(messagesEl, 'click', '.agent-chat-copy-message', (button) => {
      const source = button.closest('.agent-chat-message')!.querySelector('.agent-chat-source');
      copyToClipboardWithFeedback(button, source!.textContent);
    });
    messagesEl.querySelector('.agent-chat-load-older')?.addEventListener('click', loadOlder);
  }

  function applyRun(run: AgentChatRun) {
    if (run.status === 'queued' || run.status === 'running') {
      showStatus(run.status, run.status === 'queued' ? labels.queued : labels.running, run.cancelable);
    } else if (run.status === 'failed') {
      showStatus(run.status, run.error ? `${labels.failed}: ${run.error}` : labels.failed, false);
    } else if (run.status === 'canceled') {
      showStatus(run.status, labels.canceled, false);
    } else {
      hideElem(statusEl);
      hideElem(statusCancelEl);
    }
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
  function applyConversation(conversation: AgentChatConversation) {
    const header = document.querySelector<HTMLElement>('.agent-chat-head-title');
    if (header && elConversationID === String(conversation.id)) header.textContent = conversation.title;
    const id = String(conversation.id);
    for (const link of document.querySelectorAll<HTMLElement>('.agent-chat-card-main, .agent-chat-card')) {
      if (link.getAttribute('href')!.split('/').pop() !== id) continue;
      const label = link.querySelector<HTMLElement>('.gt-ellipsis, .agent-chat-card-name');
      // the resident session shows what it worked on instead of its own name
      if (label) label.textContent = conversation.summary || conversation.title;
    }
  }

  function connect() {
    const source = new EventSource(`${eventsUrl}?since=${mainEl!.getAttribute('data-last-message-id')!}`);

    source.addEventListener('message', (event: MessageEvent) => {
      appendMessage(JSON.parse(event.data) as AgentChatMessage);
    });
    source.addEventListener('run', (event: MessageEvent) => {
      applyRun(JSON.parse(event.data) as AgentChatRun);
    });
    source.addEventListener('conversation', (event: MessageEvent) => {
      applyConversation(JSON.parse(event.data) as AgentChatConversation);
    });
    source.addEventListener('error', () => {
      if (source.readyState === EventSource.CLOSED) return;
      source.close();
      // reconnect ourself with the latest cursor instead of letting the browser replay the stale one
      setTimeout(connect, reconnectDelay);
    });
  }

  initTranscript();
  initMessageActions();
  scrollToLastQuestion();

  composerEl.addEventListener('submit', async (event) => {
    event.preventDefault();
    try {
      const resp = await POST(messagesUrl, {data: new FormData(composerEl), headers: ajaxHeaders});
      if (!resp.ok) {
        showStatus('failed', `${labels.error} (${resp.status} ${resp.statusText})`, false);
        return;
      }
      inputEl.value = '';
    } catch (error) {
      showStatus('failed', `${labels.error} (${errorMessage(error)})`, false);
    }
  });

  connect();
}
