// The page that watches the agent: a run that changes state is marked where it is listed, so a
// reader sees the agent working without reloading the page.
const statusLabels: Record<string, string> = {
  running: 'running',
  waiting: 'waiting',
  failed: 'failed',
  canceled: 'canceled',
  queued: 'queued',
  succeeded: 'succeeded',
};

export function initAgentRuns() {
  const page = document.querySelector<HTMLElement>('.agent-runs[data-events-url]');
  if (!page || !window.EventSource) return;
  const url = page.getAttribute('data-events-url')!;

  const paint = (run: {id: number, status: string, error?: string}) => {
    const row = page.querySelector<HTMLElement>(`tr[data-run-id="${CSS.escape(String(run.id))}"]`);
    if (!row) return; // a run this page never listed: the next load will show it
    const state = row.querySelector<HTMLElement>('.agent-runs-state');
    if (state) {
      state.textContent = statusLabels[run.status] ?? run.status;
      state.className = `agent-runs-state ui tiny label agent-runs-${run.status}`;
    }
    const reason = row.querySelector<HTMLElement>('.agent-runs-reason');
    if (reason && run.error) reason.textContent = run.error;
  };

  const connect = () => {
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
  };

  connect();
  document.addEventListener('visibilitychange', () => {
    if (document.visibilityState === 'visible') connect();
  });
}
