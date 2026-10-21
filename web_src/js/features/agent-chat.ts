import {GET, POST} from '../modules/fetch.ts';
import {copyToClipboardWithFeedback} from '../modules/clipboard.ts';
import {svg} from '../svg.ts';
import {errorMessage} from '../modules/errors.ts';
import {addDelegatedEventListener, createElementFromHTML, toggleElem} from '../utils/dom.ts';

type AgentChatLabels = {queued: string, running: string, failed: string, canceled: string, error: string, copy: string, edit: string, reasoning: string};
type AgentChatDelta = {runId: number, content?: string, reasoning?: string, reset?: boolean};
type AgentChatAttachment = {id: number, name: string, sizeText: string, url: string};
type AgentChatMessage = {id: number, role: string, roleLabel: string, content: string, html: string, timeHTML: string, editedHTML?: string, attachments?: AgentChatAttachment[]};
type AgentChatRun = {id: number, conversationId: number, status: string, error: string, cancelable: boolean};
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
  function setSessionBusy(conversationID: string, isBusy: boolean) {
    const card = document.querySelector<HTMLElement>(`.agent-chat-card[data-session-id="${CSS.escape(conversationID)}"]`);
    if (!card) return;
    const existing = card.querySelector<HTMLElement>('.agent-chat-card-busy');
    if (!isBusy) {
      existing?.remove();
      return;
    }
    if (existing) return;
    const dot = createElementFromHTML(`<span class="agent-chat-card-busy" data-tooltip-content="${sessionBusyLabel}"><i></i><i></i><i></i></span>`);
    (card.querySelector('.agent-chat-card-main') ?? card).append(dot);
  }

  // without an open conversation the list still follows the runs of every conversation
  function followAllRuns() {
    const url = chatEl?.getAttribute('data-runs-url');
    if (!url || !window.EventSource || !document.querySelector('.agent-chat-card[data-session-id]')) return;
    const source = new EventSource(url);
    source.addEventListener('run', (event: MessageEvent) => {
      const run = JSON.parse(event.data) as AgentChatRun;
      setSessionBusy(String(run.conversationId), run.status === 'queued' || run.status === 'running');
    });
    source.addEventListener('error', () => {
      if (source.readyState === EventSource.CLOSED) return;
      source.close();
      setTimeout(followAllRuns, reconnectDelay);
    });
  }

  // `data-conversation-id` marks the page as a live chat, all URLs and labels are rendered by the server
  const mainEl = document.querySelector<HTMLElement>('.agent-chat-main[data-conversation-id]');
  if (!mainEl) {
    followAllRuns();
    return;
  }

  const eventsUrl = mainEl.getAttribute('data-events-url')!;
  const messagesUrl = mainEl.getAttribute('data-messages-url')!;
  const cancelUrl = mainEl.getAttribute('data-cancel-url')!;
  const labels = JSON.parse(mainEl.getAttribute('data-labels')!) as AgentChatLabels;
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

  function buildMessage(message: AgentChatMessage): HTMLElement {
    const messageEl = document.createElement('div');
    messageEl.className = `agent-chat-message ${message.role}`;
    messageEl.setAttribute('data-message-id', String(message.id));

    const metaEl = document.createElement('div');
    metaEl.className = 'agent-chat-message-meta';
    metaEl.textContent = message.roleLabel;
    if (message.editedHTML) {
      metaEl.insertAdjacentHTML('beforeend', ` · ${svg('octicon-pencil', 12)} `);
      metaEl.insertAdjacentHTML('beforeend', message.editedHTML);
    } else {
      metaEl.textContent += ' · ';
      metaEl.insertAdjacentHTML('beforeend', message.timeHTML);
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
      actionsEl.insertAdjacentHTML('beforeend', `<button type="button" class="agent-chat-edit-message show-modal" data-modal="#agent-chat-edit-modal" data-modal-form.url="${editURL}${message.id}" data-tooltip-content="${labels.edit}">${svg('octicon-pencil', 14)}</button>`);
    }

    const sourceEl = document.createElement('div');
    sourceEl.className = 'agent-chat-source tw-hidden';
    sourceEl.textContent = message.content;

    messageEl.append(metaEl, bubbleEl, actionsEl, sourceEl);
    return messageEl;
  }

  function appendMessage(message: AgentChatMessage) {
    if (messagesEl.querySelector(`[data-message-id="${CSS.escape(String(message.id))}"]`)) return;
    dropPending();

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
    el.innerHTML = `<div class="agent-chat-bubble">
      <div class="agent-chat-pending-status"><span class="agent-chat-reasoning-dots"><i></i><i></i><i></i></span><span class="agent-chat-pending-label"></span></div>
      <div class="agent-chat-live-text"></div>
    </div>`;
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

  function applyDelta(delta: AgentChatDelta) {
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
    if (delta.content) {
      ensurePending().querySelector<HTMLElement>('.agent-chat-live-text')!.textContent += delta.content;
      setPendingState('streaming', '');
    } else {
      setPendingState('reasoning', labels.reasoning); // the model thinks before it answers
    }
    // a running answer keeps the view on it, even when the question was pinned earlier
    if (atBottom || messagesEl.querySelector('.agent-chat-pending')) {
      messagesEl.scrollTop = messagesEl.scrollHeight;
    }
  }

  function applyRun(run: AgentChatRun) {
    setSessionBusy(elConversationID, run.status === 'queued' || run.status === 'running');
    if (run.status === 'queued' || run.status === 'running') {
      setBusy(true);
      const state = pendingEl?.getAttribute('data-state');
      if (run.status === 'queued' || !state || state === 'queued') {
        setPendingState(run.status === 'queued' ? 'queued' : 'running', run.status === 'queued' ? labels.queued : labels.running);
      }
      return;
    }
    setBusy(false);
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
  function applyConversation(conversation: AgentChatConversation) {
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

  function connect() {
    const source = new EventSource(`${eventsUrl}?since=${mainEl!.getAttribute('data-last-message-id')!}`);

    source.addEventListener('message', (event: MessageEvent) => {
      appendMessage(JSON.parse(event.data) as AgentChatMessage);
    });
    source.addEventListener('delta', (event: MessageEvent) => {
      applyDelta(JSON.parse(event.data) as AgentChatDelta);
    });
    source.addEventListener('run', (event: MessageEvent) => {
      const run = JSON.parse(event.data) as AgentChatRun;
      if (String(run.conversationId) === elConversationID) {
        applyRun(run);
      } else {
        setSessionBusy(String(run.conversationId), run.status === 'queued' || run.status === 'running');
      }
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
      if (!pendingEl) setPendingState('queued', labels.queued); // the run event follows, this avoids a gap
    } catch (error) {
      setBusy(false, `${labels.error} (${errorMessage(error)})`);
    }
  });

  connect();
}
