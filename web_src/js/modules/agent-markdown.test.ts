import {StreamedAnswer, renderBlocks} from './agent-markdown.ts';

// A piece of an answer drawn on its own reads as markdown, and an answer that contains markup is
// read as text: what a model was told by whoever wrote the code it read must not become markup.
describe('renderBlocks', () => {
  test('renders what an answer is written with', () => {
    const html = renderBlocks(['# A heading', '', '- one', '- two', '', 'A paragraph with `code`.', '', '```go', 'x := 1', '```'].join('\n'));
    expect(html).toContain('<h1>A heading</h1>');
    expect(html).toContain('<ul><li>one</li><li>two</li></ul>');
    expect(html).toContain('<p>A paragraph with <code>code</code>.</p>');
    expect(html).toContain('<pre><code class="language-go">x := 1</code></pre>');
  });

  test('reads a script in an answer as text', () => {
    const html = renderBlocks('<script>alert(1)</script>');
    expect(html).not.toContain('<script');
    expect(html).toContain('&lt;script&gt;');
  });

  test('follows a link only where a browser may go', () => {
    expect(renderBlocks('[a](https://example.com/x)')).toContain('<a href="https://example.com/x"');
    expect(renderBlocks('[a](javascript:alert(1))')).not.toContain('href');
    expect(renderBlocks('[a](javascript:alert(1))')).toContain('a');
  });
});

// An answer arrives in pieces. What is drawn as it arrives is what the whole answer would render
// as, and a block that is finished is left alone while the block being written grows.
describe('StreamedAnswer', () => {
  const newEl = () => {
    const el = document.createElement('div');
    document.body.append(el);
    return el;
  };

  test('draws the answer as it arrives, once it is whole', () => {
    const el = newEl();
    const answer = new StreamedAnswer(el);
    for (const piece of ['# Title', '\n\n- one', '\n- two', '\n\nDone.']) {
      answer.append(piece);
    }
    answer.finish();
    expect(el.textContent).toContain('Title');
    expect(el.querySelectorAll('h1')).toHaveLength(1);
    expect(el.querySelectorAll('li')).toHaveLength(2);
    expect(el.textContent).toContain('Done.');
  });

  test('leaves a finished block as it was drawn', async () => {
    const el = newEl();
    const answer = new StreamedAnswer(el);
    answer.append('A first block.\n\n');
    await new Promise((resolve) => requestAnimationFrame(resolve));
    const drawn = el.querySelector('.agent-markdown-block');
    expect(drawn).not.toBeNull();

    drawn!.setAttribute('data-touched', 'here');
    answer.append('A second block.\n\n');
    await new Promise((resolve) => requestAnimationFrame(resolve));

    const blocks = el.querySelectorAll('.agent-markdown-block');
    expect(blocks[0].getAttribute('data-touched')).toBe('here');
    expect(el.textContent).toContain('A second block.');
  });

  test('draws a piece that answers nothing', () => {
    const el = newEl();
    const answer = new StreamedAnswer(el);
    answer.append(' '.repeat(3));
    answer.finish();
    expect(el.querySelectorAll('.agent-markdown-block')).toHaveLength(0);
  });

  test('draws a fence that is still open as code, and closes it when it ends', () => {
    const el = newEl();
    const answer = new StreamedAnswer(el);
    answer.append('before\n\n```sh\necho ');
    answer.finish();
    expect(el.querySelector('pre code')?.textContent).toContain('echo');
    expect(el.textContent).not.toContain('```');

    answer.append('hi\n```\n');
    answer.finish();
    expect(el.querySelector('pre code')?.textContent).toContain('echo hi');
  });
});
