// Real-time agent updates for any page of the instance: one stream per page, opened where a
// page asks for it and handed back while the page is hidden, plus the mark a session card
// wears while its conversation is answering.
import {createElementFromHTML} from '../utils/dom.ts';

export type AgentAttachment = {id: number, name: string, sizeText: string, url: string, isPicture: boolean};
export type AgentMessage = {id: number, role: string, roleLabel: string, content: string, html: string, timeHTML: string, reasoning?: string, attachments?: AgentAttachment[], tool?: {name: string, args: string, result: string}};
export type AgentRun = {id: number, conversationId: number, status: string, error: string, cancelable: boolean};
export type AgentDelta = {runId: number, content?: string, reasoning?: string, reset?: boolean};
export type AgentConversation = {id: number, title: string, summary: string, icon: string};
export type AgentApproval = {id: number, runID: number, tool: string, arguments: string};

export type AgentLiveHandlers = {
  onRun?: (run: AgentRun) => void;
  onMessage?: (message: AgentMessage) => void;
  onDelta?: (delta: AgentDelta) => void;
  onConversation?: (conversation: AgentConversation) => void;
  onApproval?: (approval: AgentApproval) => void;
};

// the browser's own reconnect replays its stale cursor, so the stream is reopened here with
// the cursor the page has meanwhile
const reconnectDelay = 2000;

export type AgentLiveStream = {
  // close hands the stream back for good, for a page that is done following
  close: () => void;
  // resume reopens it after the page was hidden, when the page keeps it open on purpose
  resume: () => void;
};

// followAgentLive opens the event stream of a page and keeps it open while the page is shown.
// The url is asked for at every open, so a cursor the page moved meanwhile is used.
export function followAgentLive(url: string | (() => string), handlers: AgentLiveHandlers): AgentLiveStream {
  let source: EventSource | null = null;
  let stopped = false;

  function open() {
    if (stopped || source || document.visibilityState === 'hidden' || !window.EventSource) return;
    source = new EventSource(typeof url === 'function' ? url() : url);
    const stream = source;
    const listen = <T>(name: string, handler?: (data: T) => void) => {
      if (!handler) return;
      stream.addEventListener(name, (event: MessageEvent) => handler(JSON.parse(event.data) as T));
    };
    listen('run', handlers.onRun);
    listen('message', handlers.onMessage);
    listen('delta', handlers.onDelta);
    listen('conversation', handlers.onConversation);
    listen('approval', handlers.onApproval);
    stream.addEventListener('error', () => {
      // the browser reports a closed stream here too, and it must not be left open
      if (source !== stream) return;
      stream.close();
      source = null;
      if (stopped || document.visibilityState === 'hidden') return;
      setTimeout(open, reconnectDelay);
    });
  }

  function close() {
    source?.close();
    source = null;
  }

  // a stream holds one of the few connections a browser opens per origin, and a page that is
  // left behind keeps holding it, so a reader who switches tabs a few times starves the page
  // that comes next. The stream is handed back on the way out and picked up when shown again.
  const onVisibilityChange = () => {
    if (document.visibilityState === 'hidden') {
      close();
    } else {
      open(); // the page may come back from the back/forward cache, not just from being shown
    }
  };
  document.addEventListener('visibilitychange', onVisibilityChange);
  window.addEventListener('pagehide', close);
  window.addEventListener('pageshow', open);
  open();

  return {
    close: () => {
      stopped = true;
      close();
      document.removeEventListener('visibilitychange', onVisibilityChange);
      window.removeEventListener('pagehide', close);
      window.removeEventListener('pageshow', open);
    },
    resume: open,
  };
}

// markSessionBusy marks the card of a conversation wherever the page shows one, so a reader
// sees which sessions are answering.
export function markSessionBusy(conversationID: string, busy: boolean, label: string) {
  const card = document.querySelector<HTMLElement>(`.agent-chat-card[data-session-id="${CSS.escape(conversationID)}"]`);
  if (!card) return;
  const existing = card.querySelector<HTMLElement>('.agent-chat-card-busy');
  if (!busy) {
    existing?.remove();
    return;
  }
  if (existing) return;
  const dot = createElementFromHTML(`<span class="agent-chat-card-busy" data-tooltip-content="${label}"><i></i><i></i><i></i></span>`);
  (card.querySelector('.agent-chat-card-main') ?? card).append(dot);
}

// RunIsBusy reports whether a run still writes an answer.
export function runIsBusy(run: AgentRun): boolean {
  return run.status === 'queued' || run.status === 'running';
}
