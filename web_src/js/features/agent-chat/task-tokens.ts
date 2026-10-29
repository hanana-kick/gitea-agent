// The composer writes tasks: "/" calls a skill, "@" names a subject, and they are separate —
// either may stand alone. What the reader reads is not what is sent: the box shows the skill's
// name and the subject's own name ("@#2 CI 타임아웃 재발 조사"), while the message carries the
// command and the reference it needs ("/pr @acme/widgets#2").
//
// A token is parsed when the reader completes it from the palette or breaks the line after
// typing it, and it is not an object in the box: the caret moves through it like any text, and
// the first deletion inside the line it sits on hands the whole line back as plain text.
export type AgentTask = {
  command: string, // the skill the line calls, without the slash
  commandTitle: string, // what that skill is called in the reader's language, and what is read
  subject: string, // the reference the message needs, without the at-sign
  subjectLabel: string, // what the reader reads instead
  request: string, // the reader's own words
};

// what a card or a palette row hands over: a task with no request yet
export type AgentTaskSource = Omit<AgentTask, 'request'>;

// the tokens are two words apart, so the pills drawn around them do not run into each other;
// the mirror has to keep the very same gap, or its words drift away from the textarea's
const tokenGap = '  ';

// one token of the line, as it is read and as it is sent
type Token = {display: string, canonical: string};

// the line the tokens take, and the shape the box keeps while the task stands
export function tokenLine(task: AgentTask) {
  return [
    task.command === '' ? '' : task.commandTitle,
    task.subject === '' ? '' : `@${task.subjectLabel}`,
  ].filter((part) => part !== '').join(tokenGap);
}

// the message a task stands for: the command, the subject it needs, and the request
export function taskPayload(task: AgentTask) {
  const parts = [
    task.command === '' ? '' : `/${task.command}`,
    task.subject === '' ? '' : `@${task.subject}`,
  ].filter((part) => part !== '');
  if (task.request.trim() !== '') parts.push(task.request.trim());
  return parts.join(' ');
}

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

  const tokensOf = (kept: AgentTask): Token[] => {
    const tokens: Token[] = [];
    if (kept.command !== '') tokens.push({display: kept.commandTitle, canonical: `/${kept.command}`});
    if (kept.subject !== '') tokens.push({display: `@${kept.subjectLabel}`, canonical: `@${kept.subject}`});
    return tokens;
  };

  // paints the line: the tokens are marked, the reader's own words are not
  const paint = (kept: AgentTask, value: string) => {
    const nodes: Node[] = [];
    for (const [index, token] of tokensOf(kept).entries()) {
      if (index > 0) nodes.push(document.createTextNode(tokenGap));
      const el = document.createElement('span');
      el.className = `agent-chat-token agent-chat-token-${token.canonical.startsWith('/') ? 'command' : 'subject'}`;
      el.textContent = token.display;
      nodes.push(el);
    }
    nodes.push(document.createTextNode(value.slice(tokenLine(kept).length)));
    if (value.endsWith('\n')) nodes.push(document.createTextNode(' '));
    mirror.replaceChildren(...nodes);
    mirror.scrollTop = input.scrollTop; // a value taller than the box scrolls with it
  };

  const clear = () => {
    task = null;
    mirror.replaceChildren();
  };

  const requestOf = (value: string) => {
    const rest = value.slice(tokenLine(task as AgentTask).length);
    return rest.replace(/^\n/, '');
  };

  const sync = () => {
    if (!task) {
      mirror.replaceChildren();
      return;
    }
    // the line stands only while it is untouched: the first edit hands it back as plain text
    if (!input.value.startsWith(tokenLine(task))) {
      clear();
      return;
    }
    task = {...task, request: requestOf(input.value)}; // the words under the line are the reader's
    paint(task, input.value);
  };

  copyBox();
  input.addEventListener('input', sync);
  input.addEventListener('scroll', sync);
  // a token is not something to nibble at: the first deletion that touches one takes the
  // whole task with it, and the box is the reader's again
  input.addEventListener('beforeinput', (event) => {
    if (!task || !event.inputType.startsWith('delete')) return;
    const caret = input.selectionStart ?? 0;
    if (caret > tokenLine(task).length) return;
    event.preventDefault();
    clear();
    input.value = '';
    input.dispatchEvent(new Event('input', {bubbles: true}));
  });
  sync();

  return {
    // the task the box currently stands for, for the palette to reason about
    current: () => task,
    // puts a task in the box: the reader reads the line, writes what they want under it
    set(source: AgentTaskSource, request = '') {
      const next: AgentTask = {...source, request};
      task = next;
      const line = tokenLine(next);
      input.value = request === '' ? line : `${line}\n${request}`;
      input.dispatchEvent(new Event('input', {bubbles: true}));
      input.setSelectionRange(input.value.length, input.value.length);
      input.focus({preventScroll: true});
      return next;
    },
    sync,
    // writes the message a task stands for, and hands the box back to the reader
    apply(): string | null {
      if (!task) return null;
      const value = taskPayload(task);
      clear();
      input.value = value;
      return value;
    },
  };
}
