// The composer's box is an editable element, not a textarea, so that a picked task word can
// become a chip: a real element with its own padding and margins, standing in the text at the
// place it was written. A textarea cannot hold one — nothing but text fits inside it — and a
// copy of its text painted behind it can only pretend at the shape a chip needs.
//
// What is read is not what is sent. A chip reads as the name it carries ("이슈 검토") while
// standing for the word the message needs ("/issue-triage"), so the box answers both: what
// the reader sees, and the message that goes out.
const sigilWord = /(^|\s)([/@])(\S+)/g;

export type AgentToken = {display: string, canonical: string};

export type ComposerInput = {
  el: HTMLElement,
  text: () => string,
  setText: (text: string) => void,
  // replaces the words of the text that `resolve` knows with the chips they read as
  applyTokens: (resolve: (word: string) => AgentToken | null) => void,
  clear: () => void,
  focusToEnd: () => void,
  // the text before the caret, as the message carries it, so a word can be recognized there
  beforeCaret: () => string,
  // replaces the `chars` characters that end `keep` characters before the caret with a chip, or
  // with plain text; the caret stays where it was, so a word just finished keeps its space
  replaceBefore: (chars: number, value: AgentToken | string, keep?: number) => void,
};

// the text of a node as the message carries it: a chip is the word it stands for, a line break
// a newline
function nodeText(node: Node): string {
  switch (node.nodeType) {
    case Node.TEXT_NODE:
      return node.textContent ?? '';
    case Node.ELEMENT_NODE: {
      const element = node as HTMLElement;
      if (element.hasAttribute('data-canonical')) return element.getAttribute('data-canonical')!;
      const inner = Array.from(element.childNodes, nodeText).join('');
      if (element.tagName === 'BR') return '\n';
      // the browser breaks a line with a block of its own when it is asked to
      return element.tagName === 'DIV' || element.tagName === 'P' ? `\n${inner}` : inner;
    }
    default:
      return '';
  }
}

export function initComposerInput(form: HTMLFormElement): ComposerInput | null {
  const el = form.querySelector<HTMLElement>('.agent-chat-input');
  const field = form.querySelector<HTMLInputElement>('input[name="content"]');
  if (!el || !field) return null;

  const text = () => Array.from(el.childNodes, nodeText).join('');

  // the hidden field is the message, kept in step so that any submit carries the words
  const sync = () => {
    field.value = text();
    el.setAttribute('data-empty', field.value === '' ? 'true' : 'false');
  };
  el.addEventListener('input', sync);
  sync();

  // a chip: the name the reader reads, and the word the message carries
  const chip = (token: AgentToken): HTMLElement => {
    const node = document.createElement('span');
    node.className = token.canonical.startsWith('/') ? 'agent-chat-token agent-chat-token-command' : 'agent-chat-token agent-chat-token-subject';
    node.textContent = token.display;
    node.setAttribute('data-canonical', token.canonical);
    // a chip is not for editing: the reader deletes one whole and never types inside it
    node.contentEditable = 'false';
    return node;
  };

  // the text node the caret is in, and where in it: the only place a word is written
  const caret = (): {node: Text, offset: number} | null => {
    const selection = window.getSelection();
    if (!selection || selection.rangeCount === 0) return null;
    const range = selection.getRangeAt(0);
    if (!el.contains(range.startContainer) || range.startContainer.nodeType !== Node.TEXT_NODE) return null;
    return {node: range.startContainer as Text, offset: range.startOffset};
  };

  const placeCaret = (node: Node, offset: number) => {
    const range = document.createRange();
    range.setStart(node, offset);
    range.collapse(true);
    const selection = window.getSelection();
    selection?.removeAllRanges();
    selection?.addRange(range);
  };

  return {
    el,
    text,
    setText(value: string) {
      el.replaceChildren(...(value === '' ? [] : [document.createTextNode(value)]));
      sync();
      placeCaret(el, el.childNodes.length);
    },
    applyTokens(resolve) {
      for (const node of Array.from(el.childNodes)) {
        if (node.nodeType !== Node.TEXT_NODE) continue;
        const words = node.textContent!;
        const parts: Node[] = [];
        let at = 0;
        for (const match of words.matchAll(sigilWord)) {
          const token = resolve(match[2] + match[3]);
          if (!token) continue;
          const from = (match.index ?? 0) + match[1].length;
          if (from > at) parts.push(document.createTextNode(words.slice(at, from)));
          parts.push(chip(token));
          at = from + match[2].length + match[3].length;
        }
        if (parts.length === 0) continue;
        if (at < words.length) parts.push(document.createTextNode(words.slice(at)));
        node.replaceWith(...parts);
      }
      sync();
    },
    clear() {
      el.replaceChildren();
      sync();
    },
    focusToEnd() {
      el.focus({preventScroll: true});
      placeCaret(el, el.childNodes.length);
    },
    beforeCaret() {
      const nodes = Array.from(el.childNodes);
      const at = caret();
      if (!at) return text();
      return [...nodes.slice(0, nodes.indexOf(at.node)).map(nodeText), at.node.data.slice(0, at.offset)].join('');
    },
    replaceBefore(chars, value, keep = 0) {
      const at = caret();
      const end = at ? at.offset - keep : 0;
      if (!at || end < chars) return;
      // the replaced characters are cut out of the text node, which leaves the rest of the box
      // exactly where it was: only the words change, never the lines around them
      at.node.deleteData(end - chars, chars);
      const rest = at.node.splitText(end - chars);
      const node = typeof value === 'string' ? document.createTextNode(value) : chip(value);
      rest.parentNode!.insertBefore(node, rest);
      // the caret goes on past what was written: into the word itself, so the words right behind
      // the caret can be read again at once, or past the chip, which is not written in
      if (keep > 0) placeCaret(rest, keep);
      else if (node.nodeType === Node.TEXT_NODE) placeCaret(node, (node as Text).length);
      else placeCaret(el, Array.from(el.childNodes).indexOf(node) + 1);
      el.dispatchEvent(new Event('input'));
    },
  };
}
