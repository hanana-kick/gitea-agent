// A task is a command and a subject, and the reader's own request under them. The composer
// shows it as such: a line of two tokens, then the request. What the reader sees is not what
// is sent — the command travels with the subject it needs ("@owner/repo#2") rather than the
// label the reader reads — so the task is kept here and written into the message on submit.
//
// The plate is tied to that task, not to the shape of the text: editing the token line ends
// the task, and whatever is in the box from then on is an ordinary message.
export type AgentTask = {
  command: string, // "/pr" runs as "/pr @owner/repo#2 ..."
  subject: string, // the "@..." the command needs
  label: string, // what the reader reads in its place
  task: string, // the request the task comes with, and the reader may rewrite
};

export function initTaskTokens(form: HTMLFormElement, input: HTMLTextAreaElement) {
  const mirror = form.querySelector<HTMLElement>('.agent-chat-input-mirror');
  if (!mirror) return null;

  let task: AgentTask | null = null;

  // the mirror has to lay its text on the very box the textarea uses, or the plate lands
  // beside the words instead of behind them: take the metrics from the textarea itself
  const copyBox = () => {
    const style = getComputedStyle(input);
    const from = ['paddingTop', 'paddingRight', 'paddingBottom', 'paddingLeft', 'borderTopWidth',
      'borderRightWidth', 'borderBottomWidth', 'borderLeftWidth', 'fontFamily', 'fontSize',
      'fontWeight', 'lineHeight', 'letterSpacing', 'textIndent'] as const;
    for (const property of from) mirror.style[property] = style[property];
  };

  // the token line the task shows, and the shape the box has to keep for the task to stand
  const tokenLine = (kept: AgentTask) => `/${kept.command} ${kept.label}`;
  const bodyOf = (kept: AgentTask, value: string) => value.slice(tokenLine(kept).length).replace(/^\n/, '');

  const paint = (value: string, plates: AgentTask) => {
    const command = document.createElement('span');
    command.className = 'agent-chat-token agent-chat-token-command';
    command.textContent = `/${plates.command}`;
    const label = document.createElement('span');
    label.className = 'agent-chat-token agent-chat-token-subject';
    label.textContent = plates.label;
    const rest = document.createTextNode(value.slice(tokenLine(plates).length));
    mirror.replaceChildren(command, document.createTextNode(' '), label, rest);
    if (value.endsWith('\n')) mirror.append(document.createTextNode(' '));
  };

  const sync = () => {
    if (!task) {
      mirror.replaceChildren();
      return;
    }
    // the task stands only while its own token line is untouched
    if (!input.value.startsWith(tokenLine(task))) {
      task = null;
      mirror.replaceChildren();
      return;
    }
    paint(input.value, task);
    mirror.scrollTop = input.scrollTop; // a value taller than the box scrolls with it
  };

  // what the reader sent is not what they read: the command carries the subject it needs
  const payload = (kept: AgentTask, value: string) => {
    const body = bodyOf(kept, value).trim();
    return body === '' ? `/${kept.command} @${kept.subject}` : `/${kept.command} @${kept.subject} ${body}`;
  };

  copyBox();
  input.addEventListener('input', sync);
  input.addEventListener('scroll', sync);
  sync();

  return {
    // puts a task in the box: the reader reads it, edits the request, and sends
    set(next: AgentTask) {
      task = next;
      input.value = next.task === '' ? tokenLine(next) : `${tokenLine(next)}\n${next.task}`;
      input.dispatchEvent(new Event('input', {bubbles: true})); // the box grows to fit it
      input.setSelectionRange(input.value.length, input.value.length);
      input.focus({preventScroll: true});
    },
    // writes the message a task stands for, and hands the box back to the reader
    apply(): string | null {
      if (!task) return null;
      const kept = task;
      const value = payload(kept, input.value);
      task = null;
      mirror.replaceChildren();
      input.value = value;
      return value;
    },
  };
}
