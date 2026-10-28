// A task reads as "/skill @subject" followed by the reader's own words. The composer marks
// those two as the tokens they are, without giving up the plain textarea that editing, IME
// and the browser's own caret rely on: a mirror over the textarea tints the tokens and lets
// the textarea keep every glyph.
import type {AgentPreset} from './presets.ts';

type Token = {kind: 'command' | 'subject', text: string};

// which tokens a value carries: a command this pod knows, and the subject it names
function taskTokens(value: string, commands: Set<string>): Token[] | null {
  const match = /^\/([a-z0-9][a-z0-9_-]*)(?:\s+@(\S+))?/.exec(value);
  if (!match || !commands.has(match[1])) return null;
  const tokens: Token[] = [{kind: 'command', text: match[0].slice(0, match[1].length + 1)}];
  if (match[2]) tokens.push({kind: 'subject', text: `@${match[2]}`});
  return tokens;
}

// the mirror draws the value with the tokens wrapped; everything else stays as it was typed
function paint(mirror: HTMLElement, value: string, tokens: Token[]) {
  const parts: Node[] = [];
  let rest = value;
  for (const token of tokens) {
    const at = rest.indexOf(token.text);
    if (at > 0) parts.push(document.createTextNode(rest.slice(0, at)));
    const el = document.createElement('span');
    el.className = `agent-chat-token agent-chat-token-${token.kind}`;
    el.textContent = token.text;
    parts.push(el);
    rest = rest.slice(at + token.text.length);
  }
  parts.push(document.createTextNode(rest));
  mirror.replaceChildren(...parts);
  // a trailing newline is invisible in a block, and the caret still needs its line
  if (rest.endsWith('\n')) mirror.append(document.createTextNode(' '));
}

// initTaskTokens keeps the mirror in step with the textarea of one composer. Composing an
// IME entry leaves the value untouched, so nothing is painted on top of it.
export function initTaskTokens(form: HTMLFormElement, input: HTMLTextAreaElement, presets: AgentPreset[]) {
  const mirror = form.querySelector<HTMLElement>('.agent-chat-input-mirror');
  if (!mirror || presets.length === 0) return;
  const commands = new Set(presets.map((preset) => preset.command));

  const sync = () => {
    const tokens = input.value.startsWith('/') ? taskTokens(input.value, commands) : null;
    if (!tokens) {
      mirror.replaceChildren(); // no tokens: nothing to tint
      return;
    }
    paint(mirror, input.value, tokens);
    mirror.scrollTop = input.scrollTop; // a value taller than the box scrolls with it
  };

  // the mirror has to lay its text on the very box the textarea uses, or the tint lands
  // beside the words instead of behind them: take the metrics from the textarea itself
  const copyBox = () => {
    const style = getComputedStyle(input);
    const from = ['paddingTop', 'paddingRight', 'paddingBottom', 'paddingLeft', 'borderTopWidth',
      'borderRightWidth', 'borderBottomWidth', 'borderLeftWidth', 'fontFamily', 'fontSize',
      'fontWeight', 'lineHeight', 'letterSpacing', 'textIndent'] as const;
    for (const property of from) mirror.style[property] = style[property];
  };
  copyBox();

  input.addEventListener('input', sync); // a task written by a card or the palette lands here too
  input.addEventListener('scroll', sync);
  sync();
}
