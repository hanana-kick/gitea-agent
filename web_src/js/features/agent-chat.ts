// The agent chat page: session list on the left, transcript on the right. The pieces live
// in sibling modules — the composer, the transcript, the list — and this file wires them
// to the live event stream the server pushes.
import {GET, POST} from '../modules/fetch.ts';
import {errorMessage} from '../modules/errors.ts';
import {toggleElem} from '../utils/dom.ts';
import {followAgentLive, markSessionBusy, runIsBusy} from '../modules/agent-live.ts';
import type {AgentApproval, AgentDelta, AgentMessage, AgentRun} from '../modules/agent-live.ts';
import {initAttachmentList, initComposerKeys, initDraftComposer, fitLayout} from './agent-chat/composer.ts';
import {initPresets} from './agent-chat/presets.ts';
import {initTaskTokens} from './agent-chat/task-tokens.ts';
import {buildApproval, buildMessage, initMessageActions, initTranscriptActions, scrollToLastQuestion, scrollToTop} from './agent-chat/transcript.ts';
import type {AgentChatLabels} from './agent-chat/transcript.ts';
import {initMoreMenus, initRepoGroups, applyConversationTitle} from './agent-chat/list.ts';

export function initAgentChat() {
  fitLayout();
  // the viewport and the footer both drive the panel height
  const observer = new ResizeObserver(fitLayout);
  observer.observe(document.documentElement);
  const footer = document.querySelector<HTMLElement>('footer');
  if (footer) observer.observe(footer);
  initDraftComposer();
  initMoreMenus();
  initRepoGroups();

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
  const editURL = `${messagesUrl}/`; // (the trailing slash: the per-message URL appends the message id)
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

  // the run of this session: approvals and the tool records they answer belong to it
  let currentRunID = Number(mainEl.getAttribute('data-run-id') ?? 0);

  function appendMessage(message: AgentMessage) {
    if (messagesEl.querySelector(`[data-message-id="${CSS.escape(String(message.id))}"]`)) return;
    dropPending();
    // the answer to a tool call has arrived, so its card has served its purpose
    if (message.role === 'tool') dropApprovals(currentRunID);

    for (const el of messagesEl.querySelectorAll('.agent-chat-empty')) el.remove();

    messagesEl.append(buildMessage(message, {labels, moreLabel, editURL}));
    mainEl!.setAttribute('data-last-message-id', String(message.id));
    if (message.role === 'user') {
      // the browser keeps the caret in view, so wait for the input to settle before anchoring
      requestAnimationFrame(() => {
        const users = messagesEl.querySelectorAll<HTMLElement>('.agent-chat-message.user');
        const bubble = users[users.length - 1]?.querySelector<HTMLElement>('.agent-chat-bubble') ?? users[users.length - 1];
        // the chat input steals focus first: without it the browser pulls the caret back into view
        scrollToTop(messagesEl, bubble, () => {
          composerEl.querySelector<HTMLTextAreaElement>('.agent-chat-input')?.focus({preventScroll: true});
        });
      });
      return;
    }
    // the question stays pinned until the reader moves; once they scroll to read, follow normally
    const follow = messagesEl.scrollTop >= messagesEl.scrollHeight - messagesEl.clientHeight - 80;
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
        messagesEl.insertBefore(buildMessage(message, {labels, moreLabel, editURL}), first);
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
    const thoughtEl = buildApprovalThought(labelEl);
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

    const atBottom = messagesEl.scrollHeight - messagesEl.scrollTop - messagesEl.clientHeight <= 80;
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

  // the engine stopped waiting: the cards of that run are stale, whatever the reader did
  function dropApprovals(runID: number) {
    for (const el of messagesEl.querySelectorAll<HTMLElement>('.agent-chat-approval')) {
      if (!runID || Number(el.getAttribute('data-run-id')) === runID) el.remove();
    }
  }

  function appendApproval(approval: AgentApproval) {
    if (messagesEl.querySelector(`[data-approval-id="${CSS.escape(String(approval.id))}"]`)) return;
    const atBottom = messagesEl.scrollHeight - messagesEl.scrollTop - messagesEl.clientHeight <= 80;
    for (const el of messagesEl.querySelectorAll('.agent-chat-empty')) el.remove();
    messagesEl.append(buildApproval(approval, approvalLabels, approvalsUrl));
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

  // the stream carries this conversation and the run changes of the reader's other sessions,
  // so the list beside the transcript stays live; the cursor it reopens with brings back
  // whatever arrived while the page was hidden
  followAgentLive(() => `${eventsUrl}?since=${mainEl.getAttribute('data-last-message-id')}`, {
    onMessage: (message) => appendMessage(message),
    onApproval: (approval) => appendApproval(approval),
    onDelta: (delta) => applyDelta(delta),
    onConversation: (conversation) => applyConversationTitle(conversation, elConversationID),
    onRun: (run) => {
      if (String(run.conversationId) === elConversationID) {
        applyRun(run);
      } else {
        markSessionBusy(String(run.conversationId), runIsBusy(run), sessionBusyLabel);
      }
    },
  });

  messagesEl.querySelector('.agent-chat-load-older')?.addEventListener('click', loadOlder);
  initTranscriptActions(messagesEl);
  initMessageActions(messagesEl);
  scrollToLastQuestion(messagesEl);
  // older messages can carry pictures; ask for them now, before anything scrolls
  for (const image of messagesEl.querySelectorAll('.agent-chat-attachment-image img[loading="lazy"]')) {
    image.removeAttribute('loading');
  }
  const clearAttachments = initAttachmentList(composerEl);
  const tasks = initTaskTokens(composerEl, inputEl);
  if (tasks) {
    // the task is written as its command before the form is read for sending
    composerEl.addEventListener('submit', () => tasks.apply(), {capture: true});
  }
  initComposerKeys(composerEl, inputEl, initPresets(composerEl, inputEl, tasks ?? {set: () => {}}));

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

// the fold of the pending block: hidden until a thought arrives
function buildApprovalThought(labelEl: HTMLElement) {
  const el = document.createElement('details');
  el.className = 'agent-chat-fold';
  const summaryEl = document.createElement('summary');
  summaryEl.className = 'agent-chat-fold-summary';
  summaryEl.append(labelEl);
  const bodyEl = document.createElement('div');
  bodyEl.className = 'agent-chat-fold-body';
  bodyEl.append(document.createElement('pre'));
  el.append(summaryEl, bodyEl);
  el.classList.add('agent-chat-thought', 'tw-hidden'); // no fold until there is a thought
  return el;
}
