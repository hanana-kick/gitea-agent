// What an agent answers with is markdown, and it is written a piece at a time. This renders it as
// it arrives, without ever stopping to render the whole answer again: a burst of pieces is drawn
// once, in the frame the browser is drawing anyway; a block that is finished is rendered the once
// and then left alone; and only the block still being written is rendered again.
//
// The answer the server renders when the run is over is what the reader keeps: this is what they
// read while it is being written, so it covers what an answer is written with and no more.

const fencePattern = /^ {0,3}(`{3,}|~{3,})(.*)$/;
const headingPattern = /^(#{1,6})\s+(.*)$/;
const listPattern = /^\s*([-*+]|\d+[.)])\s+/;
const quotePattern = /^ {0,3}>\s?/;
const rulePattern = /^ {0,3}([-*_])\s*(?:\1\s*){2,}$/;

/** StreamedAnswer renders one answer as it arrives, into one element. */
export class StreamedAnswer {
  private raw = '';
  private rendered = 0; // how much of raw is drawn, and finished
  private tail: HTMLElement | undefined;
  private frame = 0;

  constructor(private readonly el: HTMLElement) {}

  /** append takes the next piece of the answer, drawn when the browser draws next. */
  append(piece: string): void {
    this.raw += piece;
    if (this.frame) return;
    this.frame = requestAnimationFrame(() => {
      this.frame = 0;
      this.draw();
    });
  }

  /** finish draws what is left at once, for an answer that has stopped arriving for now. */
  finish(): void {
    if (this.frame) {
      cancelAnimationFrame(this.frame);
      this.frame = 0;
    }
    this.draw();
  }

  private draw(): void {
    const done = finishedUpTo(this.raw, this.rendered);
    if (done > this.rendered) {
      const html = renderBlocks(this.raw.slice(this.rendered, done));
      if (html) {
        const block = document.createElement('div');
        block.className = 'agent-markdown-block markup';
        block.innerHTML = html;
        this.el.append(block);
      }
      this.rendered = done;
    }

    const rest = this.raw.slice(this.rendered);
    if (!rest.trim()) {
      this.tail?.remove();
      this.tail = undefined;
      return;
    }
    if (!this.tail) {
      this.tail = document.createElement('div');
      this.tail.className = 'agent-markdown-block agent-markdown-tail markup';
      this.el.append(this.tail);
    }
    this.tail.innerHTML = renderBlocks(rest);
  }
}

// finishedUpTo walks an answer from where it was left and reports how much of it is finished: a
// blank line outside a fence ends a block, and so does a fence that is closed. What follows is the
// block still being written, which is drawn again as it grows, from its own first line, so a fence
// that is open keeps what it holds.
function finishedUpTo(raw: string, from: number): number {
  let offset = from; // where the line being read starts
  let blockStart = from; // where the block being written starts
  let writing = false;
  let fence = '';

  for (const line of raw.slice(from).split('\n')) {
    // the last piece of a split answer is what follows its final newline, if there is one
    const next = Math.min(offset + line.length + 1, raw.length);
    const trimmed = line.trim();

    if (!fence && !trimmed) {
      if (writing) {
        writing = false;
      }
      offset = next;
      blockStart = next;
      continue;
    }
    if (!writing) {
      writing = true;
      blockStart = offset;
    }
    if (!fence) {
      const opener = fencePattern.exec(line);
      if (opener) fence = opener[1][0].repeat(3);
    } else if (trimmed.startsWith(fence)) {
      // the line that closes a fence finishes the block it holds
      fence = '';
      writing = false;
      blockStart = next;
    }
    offset = next;
  }
  return writing ? blockStart : offset;
}

const escapeHtml = (text: string): string =>
  text.replaceAll('&', '&amp;').replaceAll('<', '&lt;').replaceAll('>', '&gt;').replaceAll('"', '&quot;');

// inline renders what one line of an answer is written with. The text is escaped first, so an
// answer is read as text wherever it came from: a link is followed only where a browser may go.
function inline(text: string): string {
  let html = escapeHtml(text);
  html = html.replace(/`([^`]+)`/g, '<code>$1</code>');
  html = html.replace(/\*\*([^*]+)\*\*/g, '<strong>$1</strong>');
  html = html.replace(/(^|\W)\*([^*\n]+)\*(?=\W|$)/g, '$1<em>$2</em>');
  html = html.replace(/(^|\W)_([^_\n]+)_(?=\W|$)/g, '$1<em>$2</em>');
  html = html.replace(/\[([^\]]+)\]\(([^)\s]+)\)/g, (_match, label: string, href: string) => {
    const url = /^(https?:|mailto:|\/|#)/i.test(href.trim()) ? escapeHtml(href.trim()) : '';
    return url ? `<a href="${url}" rel="nofollow noopener">${label}</a>` : label;
  });
  html = html.replace(/(^|[\s(])(https?:\/\/[^\s<)]+)/g, (_match, before: string, url: string) => {
    return `${before}<a href="${escapeHtml(url)}" rel="nofollow noopener">${url}</a>`;
  });
  return html;
}

/** renderBlocks renders markdown as HTML, one block at a time, with no HTML of its own. */
export function renderBlocks(text: string): string {
  const lines = text.split('\n');
  const out: string[] = [];
  let i = 0;

  while (i < lines.length) {
    const line = lines[i];
    if (!line.trim()) {
      i++;
      continue;
    }

    const fence = fencePattern.exec(line);
    if (fence) {
      const closer = fence[1][0].repeat(3);
      const language = fence[2].trim().split(/\s+/)[0];
      const body: string[] = [];
      i++;
      while (i < lines.length && !lines[i].trim().startsWith(closer)) {
        body.push(lines[i]);
        i++;
      }
      i++; // the closing fence, or the end of what has arrived
      const attribute = language ? ` class="language-${escapeHtml(language)}"` : '';
      out.push(`<pre><code${attribute}>${escapeHtml(body.join('\n'))}</code></pre>`);
      continue;
    }

    const heading = headingPattern.exec(line);
    if (heading) {
      const level = heading[1].length;
      out.push(`<h${level}>${inline(heading[2])}</h${level}>`);
      i++;
      continue;
    }
    if (rulePattern.test(line)) {
      out.push('<hr>');
      i++;
      continue;
    }

    if (quotePattern.test(line)) {
      const quoted: string[] = [];
      while (i < lines.length && quotePattern.test(lines[i])) {
        quoted.push(lines[i].replace(quotePattern, ''));
        i++;
      }
      out.push(`<blockquote>${quoted.map(inline).join('<br>')}</blockquote>`);
      continue;
    }

    const item = listPattern.exec(line);
    if (item) {
      const ordered = /\d/.test(item[1]);
      const items: string[] = [];
      while (i < lines.length) {
        const next = listPattern.exec(lines[i]);
        if (!next || /\d/.test(next[1]) !== ordered) break;
        items.push(lines[i].replace(listPattern, ''));
        i++;
      }
      const tag = ordered ? 'ol' : 'ul';
      out.push(`<${tag}>${items.map((entry) => `<li>${inline(entry)}</li>`).join('')}</${tag}>`);
      continue;
    }

    const paragraph: string[] = [];
    while (i < lines.length && lines[i].trim() && !isBlockStart(lines[i])) {
      paragraph.push(lines[i]);
      i++;
    }
    out.push(`<p>${paragraph.map(inline).join('<br>')}</p>`);
  }
  return out.join('');
}

// isBlockStart reports whether a line begins something rather than continuing a paragraph.
function isBlockStart(line: string): boolean {
  return (
    fencePattern.test(line) ||
    headingPattern.test(line) ||
    rulePattern.test(line) ||
    quotePattern.test(line) ||
    listPattern.test(line)
  );
}
