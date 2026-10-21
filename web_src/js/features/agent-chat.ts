import {POST} from '../modules/fetch.ts';
import {errorMessage} from '../modules/errors.ts';
import {hideElem, showElem, toggleElem} from '../utils/dom.ts';

type AgentChatLabels = {queued: string, running: string, failed: string, canceled: string, cancel: string, error: string};
type AgentChatMessage = {id: number, role: string, roleLabel: string, content: string, timeHTML: string};
type AgentChatRun = {id: number, status: string, error: string, cancelable: boolean};
type AgentChatConversation = {id: number, title: string, icon: string};

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

  function appendMessage(message: AgentChatMessage) {
    if (messagesEl.querySelector(`[data-message-id="${CSS.escape(String(message.id))}"]`)) return;

    const atBottom = messagesEl.scrollHeight - messagesEl.scrollTop - messagesEl.clientHeight <= bottomThreshold;
    for (const el of messagesEl.querySelectorAll('.agent-chat-empty')) el.remove();

    const messageEl = document.createElement('div');
    messageEl.className = `agent-chat-message ${message.role}`;
    messageEl.setAttribute('data-message-id', String(message.id));

    const metaEl = document.createElement('div');
    metaEl.className = 'agent-chat-message-meta';
    metaEl.textContent = `${message.roleLabel} · `;
    metaEl.insertAdjacentHTML('beforeend', message.timeHTML);

    const bubbleEl = document.createElement('div');
    bubbleEl.className = 'agent-chat-bubble';
    bubbleEl.textContent = message.content;

    messageEl.append(metaEl, bubbleEl);
    messagesEl.append(messageEl);
    mainEl!.setAttribute('data-last-message-id', String(message.id));
    if (atBottom) messagesEl.scrollTop = messagesEl.scrollHeight;
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
      if (label) label.textContent = conversation.title;
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
