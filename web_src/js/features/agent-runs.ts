// The page that watches the agent: a run that changes state is marked where it is listed, so a
// reader sees the agent working without reloading the page.
import {queuePlaceText} from '../modules/agent-live.ts';
import type {AgentQueueLabels, AgentRun} from '../modules/agent-live.ts';

// the colour a state is shown in, whatever language its word is in
const statusColors: Record<string, string> = {
  queued: '', waiting: 'orange', running: 'blue', succeeded: 'green', failed: 'red', canceled: 'grey',
};

export function initAgentRuns() {
  const container = document.querySelector<HTMLElement>('.agent-runs[data-events-url]');
  if (!container || !window.EventSource) return;
  const page: HTMLElement = container;
  const url = page.getAttribute('data-events-url')!;
  const labels = JSON.parse(page.getAttribute('data-labels') ?? '{}') as Record<string, string> & AgentQueueLabels;

  // the runs still waiting, so their wait can keep growing while nothing else changes
  const waiting = new Map<number, AgentRun>();
  let ticker = 0;

  function rowOf(runID: number): HTMLElement | null {
    return page.querySelector<HTMLElement>(`tr[data-run-id="${CSS.escape(String(runID))}"]`);
  }

  function paintQueue(row: HTMLElement, run: AgentRun) {
    const place = row.querySelector<HTMLElement>('.agent-runs-queue');
    if (!place) return;
    const text = run.status === 'queued' ? queuePlaceText(labels, run) : '';
    place.textContent = text;
    place.hidden = text === '';
  }

  function paint(run: AgentRun) {
    const row = rowOf(run.id);
    if (!row) return; // a run this page never listed: the next load will show it
    const state = row.querySelector<HTMLElement>('.agent-runs-status');
    if (state) {
      const word = labels[run.status] ?? run.status;
      const color = statusColors[run.status];
      state.textContent = word;
      state.className = `agent-runs-status ui tiny${color ? ` ${color}` : ''} label`;
    }
    paintQueue(row, run);
    const reason = row.querySelector<HTMLElement>('.agent-runs-reason');
    if (reason && run.error) reason.textContent = run.error;
    // a run that is over is not for stopping: the control it had goes
    const stop = row.querySelector<HTMLElement>('.agent-runs-stop');
    if (stop && !run.cancelable) stop.closest('form')?.remove();

    if (run.status === 'queued') waiting.set(run.id, run);
    else waiting.delete(run.id);
    startTicker();
  }

  // the runs in the queue wait longer than their last event says
  function startTicker() {
    if (ticker || waiting.size === 0) return;
    ticker = window.setInterval(() => {
      if (waiting.size === 0) {
        window.clearInterval(ticker);
        ticker = 0;
        return;
      }
      for (const [id, run] of waiting) {
        const row = rowOf(id);
        if (row) paintQueue(row, run);
      }
    }, 5000);
  }

  function connect() {
    const source = new EventSource(url);
    source.addEventListener('message', (event) => {
      try {
        paint(JSON.parse((event as MessageEvent<string>).data));
      } catch {
        // a keep-alive or a partial frame: the next one carries the change
      }
    });
    source.addEventListener('error', () => {
      // the stream is reopened when the page is back in front of its reader
      source.close();
    });
  }

  connect();
  document.addEventListener('visibilitychange', () => {
    if (document.visibilityState === 'visible') connect();
  });
}
