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
  // the two are separate, so neither leads: the line keeps the order the reader wrote them in
  subjectFirst: boolean,
};

// what a card or a palette row hands over: a task with no request yet
export type AgentTaskSource = Omit<AgentTask, 'request'>;

// the tokens are two words apart, so the pills drawn around them do not run into each other;
// the mirror has to keep the very same gap, or its words drift away from the textarea's
const tokenGap = '  ';

// one token of the line, as it is read and as it is sent
type Token = {display: string, canonical: string};

// the tokens of a task in the order they are read, and the one the reader wrote first
function tokensOf(task: AgentTask): Token[] {
  const command: Token | null = task.command === '' ? null : {display: task.commandTitle, canonical: `/${task.command}`};
  const subject: Token | null = task.subject === '' ? null : {display: `@${task.subjectLabel}`, canonical: `@${task.subject}`};
  const tokens = task.subjectFirst ? [subject, command] : [command, subject];
  return tokens.filter((token) => token !== null);
}

// where each token sits in the line the reader reads: a deletion takes the one it lands in
export function tokenSpans(task: AgentTask) {
  const spans: {kind: 'command' | 'subject', to: number}[] = [];
  let at = 0;
  for (const [index, token] of tokensOf(task).entries()) {
    if (index > 0) at += tokenGap.length; // the gap belongs to the token before it
    at += token.display.length;
    spans.push({kind: token.canonical.startsWith('/') ? 'command' : 'subject', to: at});
  }
  return spans;
}

// the line the tokens take, and the shape the box keeps while the task stands
export function tokenLine(task: AgentTask) {
  return tokensOf(task).map((token) => token.display).join(tokenGap);
}

// the message a task stands for: the command, the subject it needs, and the request
export function taskPayload(task: AgentTask) {
  const parts = tokensOf(task).map((token) => token.canonical);
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
  // A token is not something to nibble at: a deletion that lands on one takes it whole. The
  // two are separate jobs, so the one the caret is not in stays, and the reader's own words
  // are never touched.
  input.addEventListener('beforeinput', (event) => {
    if (!task || !event.inputType.startsWith('delete')) return;
    const caret = input.selectionStart ?? 0;
    if (caret > tokenLine(task).length) return;
    event.preventDefault();
    const spans = tokenSpans(task);
    const hit = spans.find((span) => caret <= span.to) ?? spans[spans.length - 1];
    const kept = hit.kind === 'command' ?
      {...task, command: '', commandTitle: ''} :
      {...task, subject: '', subjectLabel: ''};
    const line = tokenLine(kept);
    if (line === '') {
      clear();
      input.value = kept.request.trim();
      input.setSelectionRange(0, 0);
    } else {
      task = kept;
      input.value = kept.request.trim() === '' ? line : `${line}\n${kept.request}`;
      input.setSelectionRange(line.length, line.length);
    }
    input.dispatchEvent(new Event('input', {bubbles: true})); // the line is read again
  });
  sync();

  return {
    // the task the box currently stands for, for the palette to reason about
    current: () => task,
    // puts a task in the box: the reader reads the line, writes what they want under it
    set(source: AgentTaskSource, request = '') {
      const next: AgentTask = {...source, request};
      task = next;
      // the words the reader writes next belong under the line, not glued to it
      input.value = `${tokenLine(next)}\n${request}`;
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
