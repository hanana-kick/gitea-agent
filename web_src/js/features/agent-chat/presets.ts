// What this agent can be asked to do, written once and reached three ways: the cards above the
// composer, the "/" that calls a skill, and the "@" that names a subject. The two sigils are
// separate — either may stand alone — and a line that carries them is parsed into a task when
// it is completed from the palette or broken by the reader.
import {tokenLine} from './task-tokens.ts';
import type {AgentTask, AgentTaskSource} from './task-tokens.ts';

export type AgentPresetTarget = {label: string, subject: string, task: string};

export type AgentPreset = {
  key: string,
  command: string,
  title: string,
  desc: string,
  icon: string,
  skill: string,
  subject: string,
  task: string,
  targets: AgentPresetTarget[],
  needsTarget: boolean,
};

// one row of the palette the reader walks through
type AgentPresetEntry = {label: string, hint: string, insert: () => void};

// what the composer offers a task host: the plate, and the task it carries
type AgentTaskHost = {
  current: () => AgentTask | null,
  set: (source: AgentTaskSource, request?: string) => AgentTask,
  sync: () => void,
};

const commandSigil = '/';
const subjectSigil = '@';

export function readPresets(form: HTMLFormElement): AgentPreset[] {
  const raw = form.getAttribute('data-presets');
  if (!raw) return [];
  try {
    // a card that takes no subject arrives with a null list, so every card reads alike
    const parsed = JSON.parse(raw) as AgentPreset[];
    return parsed.map((preset) => ({...preset, targets: preset.targets ?? []}));
  } catch {
    return [];
  }
}

// the word the caret is in, when it is a sigil word: what the palette completes
function sigilWord(value: string, caret: number) {
  const before = value.slice(0, caret);
  const match = /(^|\s)([/@])(\S*)$/.exec(before);
  if (!match) return null;
  return {sigil: match[2], word: match[3], start: before.length - match[3].length - 1, end: caret};
}

export function initPresets(form: HTMLFormElement, input: HTMLTextAreaElement, tasks: AgentTaskHost) {
  const presets = readPresets(form);
  const paletteEl = form.querySelector<HTMLElement>('.agent-chat-commands');
  if (presets.length === 0 || !paletteEl) return {keyDown: (_event: KeyboardEvent) => false};
  const palette = paletteEl; // the palette is there from here on

  // every subject this agent can be aimed at, named once: the tasks share them
  const subjects = new Map<string, {label: string, hint: string}>();
  for (const preset of presets) {
    if (preset.subject !== '') subjects.set(preset.subject, {label: preset.subject, hint: preset.title});
    for (const target of preset.targets) subjects.set(target.subject, {label: target.label, hint: preset.title});
  }
  const skills = new Map(presets.map((preset) => [preset.command, preset.title]));

  const sourceOf = (command: string, subject: string): AgentTaskSource => ({
    command,
    commandTitle: command === '' ? '' : skills.get(command) ?? command,
    subject,
    subjectLabel: subject === '' ? '' : subjects.get(subject)?.label ?? subject,
  });

  // the tasks the cards hand over, by the command they call
  const presetOf = (command: string) => presets.find((preset) => preset.command === command);

  // the cards: pick a subject, then hand that task to the composer
  for (const card of form.closest('.agent-chat-main')!.querySelectorAll<HTMLElement>('.agent-chat-preset')) {
    const run = card.querySelector<HTMLButtonElement>('.agent-chat-preset-run');
    if (!run) continue;
    run.addEventListener('click', () => {
      const option = card.querySelector<HTMLOptionElement>('.agent-chat-preset-target option:checked');
      const preset = presetOf(card.getAttribute('data-command')!);
      if (!preset) return;
      const subject = option?.getAttribute('data-subject') ?? card.getAttribute('data-subject')!;
      tasks.set(sourceOf(preset.command, subject), option?.getAttribute('data-task') ?? card.getAttribute('data-task')!);
      close();
    });
  }

  let entries: AgentPresetEntry[] = [];
  let active = 0;

  function paint() {
    const rows = entries.map((entry, index) => {
      const el = document.createElement('button');
      el.type = 'button';
      el.className = index === active ? 'agent-chat-command active' : 'agent-chat-command';
      el.setAttribute('role', 'option');
      el.setAttribute('aria-selected', String(index === active));
      const label = document.createElement('span');
      label.className = 'agent-chat-command-label';
      label.textContent = entry.label;
      const hint = document.createElement('span');
      hint.className = 'agent-chat-command-hint';
      hint.textContent = entry.hint;
      el.append(label, hint);
      // the caret must stay in the input, so the palette answers the press itself
      el.addEventListener('mousedown', (event) => event.preventDefault());
      el.addEventListener('click', () => {
        entry.insert();
        close();
      });
      return el;
    });
    palette.replaceChildren(...rows);
    palette.classList.toggle('tw-hidden', rows.length === 0);
  }

  function close() {
    entries = [];
    active = 0;
    paint();
  }

  const show = (next: AgentPresetEntry[]) => {
    entries = next;
    active = Math.min(active, Math.max(0, next.length - 1));
    paint();
  };

  // breaks the line the reader is on and parses what it carries: a skill, a subject, or both
  // parses what one line carries — a skill, a subject, or both — and what the reader wrote
  // after them on that line
  function parse(line: string): {source: AgentTaskSource, rest: string} | null {
    const text = line.trimEnd();
    const match = /^\/([a-z0-9][a-z0-9_-]*)(?:\s+@(\S+))?(?:\s+([\s\S]*))?$/.exec(text);
    if (!match) {
      const subjectOnly = /^@(\S+)(?:\s+([\s\S]*))?$/.exec(text);
      if (!subjectOnly) return null;
      if (subjects.size > 0 && !subjects.has(subjectOnly[1])) return null;
      return {source: sourceOf('', subjectOnly[1]), rest: (subjectOnly[2] ?? '').trim()};
    }
    const [, command, subject, rest] = match;
    // a session with no repository knows no subjects, so a subject it names is its own to
    // name; where the list is known, only what is on it is read as a token
    if (skills.size > 0 && !skills.has(command)) return null;
    if (subject !== undefined && subjects.size > 0 && !subjects.has(subject)) return null;
    return {source: sourceOf(command, subject ?? ''), rest: (rest ?? '').trim()};
  }

  // what the caret is typing decides what is offered: a skill after "/", a subject after "@"
  const offer = () => {
    const word = sigilWord(input.value, input.selectionStart ?? input.value.length);
    if (!word) {
      close();
      return;
    }
    const typed = word.word.toLowerCase();
    if (word.sigil === commandSigil) {
      show(presets
        .filter((preset) => preset.command.toLowerCase().startsWith(typed))
        .map((preset) => ({
          label: `${commandSigil}${preset.command}`,
          hint: preset.title,
          insert: () => insertAt(word, `${commandSigil}${preset.command}`),
        })));
      return;
    }
    show([...subjects]
      .filter(([subject, named]) => subject.toLowerCase().includes(typed) || named.label.toLowerCase().includes(typed))
      .map(([subject, named]) => ({
        label: `${subjectSigil}${named.label}`,
        hint: named.hint,
        insert: () => insertAt(word, `${subjectSigil}${subject}`),
      })));
  };

  // completing a word writes it, then reads the line: what the palette wrote is a whole task
  function insertAt(word: {start: number, end: number}, text: string) {
    const value = input.value;
    const next = `${value.slice(0, word.start)}${text}${value.slice(word.end)}`;
    input.value = next;
    input.setSelectionRange(word.start + text.length, word.start + text.length);
    input.dispatchEvent(new Event('input', {bubbles: true}));
    readLine(true);
  }

  // A line that carries a whole task is read as one: as soon as the palette completes it, as
  // soon as the reader finishes a word with a space, and when they break the line after it.
  function readLine(completed = false) {
    if (tasks.current()) {
      tasks.sync();
      return;
    }
    const value = input.value;
    const cut = value.indexOf('\n');
    const line = cut === -1 ? value : value.slice(0, cut);
    if (!completed && cut === -1 && !line.endsWith(' ')) return; // the word is still being typed
    const parsed = parse(line);
    if (!parsed) return;
    const below = cut === -1 ? '' : value.slice(cut + 1);
    const request = [parsed.rest, below].filter((part) => part.trim() !== '').join('\n');
    const next = tasks.set(parsed.source, request);
    const caret = tokenLine(next).length + (request === '' ? 0 : 1 + request.length);
    input.setSelectionRange(caret, caret);
  }

  input.addEventListener('input', () => {
    offer();
    readLine();
  });

  const move = (step: number) => {
    if (entries.length === 0) return;
    active = (active + step + entries.length) % entries.length;
    paint();
    palette.querySelectorAll<HTMLElement>('.agent-chat-command')[active]?.scrollIntoView({block: 'nearest'});
  };

  return {
    // true when the key belonged to the palette, so the composer leaves it alone
    keyDown: (event: KeyboardEvent) => {
      if (entries.length === 0) return false;
      switch (event.key) {
        case 'ArrowDown':
          move(1);
          return true;
        case 'ArrowUp':
          move(-1);
          return true;
        case 'Enter':
        case 'Tab':
          entries[active].insert();
          close();
          return true;
        case 'Escape':
          close();
          return true;
      }
      return false;
    },
  };
}
