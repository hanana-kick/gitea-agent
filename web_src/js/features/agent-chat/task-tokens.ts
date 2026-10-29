// A task is written where the reader writes it: the word they picked — "/pr", "@acme/widgets#2"
// — is replaced in place by the name they read, and the rest of the text is left exactly as it
// was. Nothing is moved, nothing leads: the tokens stand in the text at their own positions,
// in whatever order they were written.
//
// What is read is not what is sent. The plate keeps what each replaced name stands for, and
// writes it back into the message on the way out.
const sigilWordPattern = (words: string[]) =>
  new RegExp(words.map((word) => word.replaceAll(/[.*+?^${}()|[\]\\]/g, '\\$&')).join('|'), 'g');

// a name the reader reads, and the word it stands for
export type AgentToken = {display: string, canonical: string};

// one occurrence of a token in the text
type AgentTokenSpan = {token: AgentToken, from: number, to: number};

export function initTaskTokens(form: HTMLFormElement, input: HTMLTextAreaElement) {
  const mirror = form.querySelector<HTMLElement>('.agent-chat-input-mirror');
  if (!mirror) return null;

  // what each name in the text stands for; a name the reader deletes is forgotten with it
  const tokens = new Map<string, AgentToken>();

  // the mirror has to lay its text on the very box the textarea uses, or the plate lands
  // beside the words instead of behind them: take the metrics from the textarea itself
  const copyBox = () => {
    const style = getComputedStyle(input);
    const from = ['paddingTop', 'paddingRight', 'paddingBottom', 'paddingLeft', 'borderTopWidth',
      'borderRightWidth', 'borderBottomWidth', 'borderLeftWidth', 'fontFamily', 'fontSize',
      'fontWeight', 'lineHeight', 'letterSpacing', 'textIndent'] as const;
    for (const property of from) mirror.style[property] = style[property];
  };

  // where the names the reader reads stand, in the order the text carries them
  const spans = (): AgentTokenSpan[] => {
    const value = input.value;
    if (tokens.size === 0) return [];
    const found: AgentTokenSpan[] = [];
    const pattern = sigilWordPattern(Array.from(tokens.keys()));
    for (const match of value.matchAll(pattern)) {
      const token = tokens.get(match[0]);
      if (token) found.push({token, from: match.index, to: match.index + match[0].length});
    }
    return found;
  };

  const paint = () => {
    const value = input.value;
    const nodes: Node[] = [];
    let at = 0;
    for (const span of spans()) {
      if (span.from > at) nodes.push(document.createTextNode(value.slice(at, span.from)));
      const el = document.createElement('span');
      el.className = `agent-chat-token agent-chat-token-${span.token.canonical.startsWith('/') ? 'command' : 'subject'}`;
      el.textContent = span.token.display;
      nodes.push(el);
      at = span.to;
    }
    nodes.push(document.createTextNode(value.slice(at)));
    if (value.endsWith('\n')) nodes.push(document.createTextNode(' '));
    mirror.replaceChildren(...nodes);
    mirror.scrollTop = input.scrollTop; // a value taller than the box scrolls with it
  };

  copyBox();
  input.addEventListener('input', paint);
  input.addEventListener('scroll', paint);
  // a name is not something to nibble at: a deletion that lands on one takes it whole, and
  // the words around it are never touched
  input.addEventListener('beforeinput', (event) => {
    if (!event.inputType.startsWith('delete')) return;
    const caret = input.selectionStart ?? 0;
    const span = spans().find((one) => caret > one.from && caret <= one.to);
    if (!span) return;
    event.preventDefault();
    input.value = `${input.value.slice(0, span.from)}${input.value.slice(span.to)}`;
    input.setSelectionRange(span.from, span.from);
    input.dispatchEvent(new Event('input', {bubbles: true}));
  });
  paint();

  return {
    // the word the reader picked is replaced by the name they read, where they wrote it
    replace(at: number, end: number, token: AgentToken): void {
      tokens.set(token.display, token);
      input.value = `${input.value.slice(0, at)}${token.display}${input.value.slice(end)}`;
      input.setSelectionRange(at + token.display.length, at + token.display.length);
      input.dispatchEvent(new Event('input', {bubbles: true}));
    },
    // what a name stands for, when the reader picked one that is already in the text
    tokenOf: (display: string) => tokens.get(display),
    // the message: every name written back as the word it stands for
    payload(): string {
      let value = input.value;
      for (const span of spans().reverse()) {
        value = `${value.slice(0, span.from)}${span.token.canonical}${value.slice(span.to)}`;
      }
      return value;
    },
    // the box is the reader's again
    reset(): void {
      tokens.clear();
      mirror.replaceChildren();
    },
  };
}
