// What this agent can be asked to do, written once and reached three ways: the cards above the
// composer, the "/" that calls a skill, and the "@" that names a subject. The two sigils are
// separate — either may stand alone — and a line that carries them is parsed into a task when
// it is completed from the palette or broken by the reader.
import type {AgentToken} from './task-tokens.ts';

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

// what the composer offers a task host: a word is replaced where it stands
type AgentTaskHost = {
  replace: (at: number, end: number, token: AgentToken) => void,
  tokenOf: (display: string) => AgentToken | undefined,
  payload: () => string,
  reset: () => void,
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

// what every task word of this page stands for, for the transcript to read a message the way
// its writer read it: "/issue-triage" reads as the skill's name, "@acme/widgets#2" as what it names
export function tokenNames(form: HTMLFormElement): Map<string, string> {
  const names = new Map<string, string>();
  for (const preset of readPresets(form)) {
    names.set(`${commandSigil}${preset.command}`, preset.title);
    // a subject reads with the at-sign that names it, exactly as the composer draws it
    if (preset.subject !== '') names.set(`${subjectSigil}${preset.subject}`, `${subjectSigil}${preset.subject}`);
    for (const target of preset.targets) names.set(`${subjectSigil}${target.subject}`, `${subjectSigil}${target.label}`);
  }
  return names;
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

  // the tasks the cards hand over, by the command they call
  const presetOf = (command: string) => presets.find((preset) => preset.command === command);

  // the cards: pick a subject, and the task is written into the box as the names it reads by
  for (const card of form.closest('.agent-chat-main')!.querySelectorAll<HTMLElement>('.agent-chat-preset')) {
    const run = card.querySelector<HTMLButtonElement>('.agent-chat-preset-run');
    if (!run) continue;
    run.addEventListener('click', () => {
      const option = card.querySelector<HTMLOptionElement>('.agent-chat-preset-target option:checked');
      const preset = presetOf(card.getAttribute('data-command')!);
      if (!preset) return;
      const subject = option?.getAttribute('data-subject') ?? card.getAttribute('data-subject')!;
      const request = (option?.getAttribute('data-task') ?? card.getAttribute('data-task')!) || '';
      const words = [
        tokenOf(commandSigil, preset.command),
        subject === '' ? null : tokenOf(subjectSigil, subject),
      ].filter((token) => token !== null);
      // the box takes the words, and the plate turns each of them into the name it reads by
      input.value = `${words.map((token) => token.canonical).join(' ')}${request === '' ? '' : `\n${request}`}`;
      input.dispatchEvent(new Event('input', {bubbles: true}));
      let at = 0;
      for (const token of words) {
        const found = input.value.indexOf(token.canonical, at);
        if (found === -1) continue;
        tasks.replace(found, found + token.canonical.length, token);
        at = found + token.display.length;
      }
      input.setSelectionRange(input.value.length, input.value.length);
      input.focus({preventScroll: true});
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
  // What a word stands for: the name the reader reads, and the word that is sent. A skill
  // reads by its own name, a subject by what it names; where nothing is known — a session
  // with no repository — a subject is its own name.
  function tokenOf(sigil: string, word: string): AgentToken | null {
    if (sigil === commandSigil) {
      if (skills.size > 0 && !skills.has(word)) return null;
      return {display: skills.get(word) ?? word, canonical: `${commandSigil}${word}`};
    }
    if (subjects.size > 0 && !subjects.has(word)) return null;
    const named = subjects.get(word);
    return {display: `${subjectSigil}${named?.label ?? word}`, canonical: `${subjectSigil}${word}`};
  }

  // the word the reader just finished, wherever they wrote it: it ends at a space, at a line
  // break, or at the word the palette completed
  function finishedWord(completed: boolean) {
    const value = input.value;
    const pattern = completed ? /(^|\s)([/@])(\S+)$/g : /(^|\s)([/@])(\S+)\s/g;
    let match: RegExpExecArray | null = null;
    for (const found of value.matchAll(pattern)) match = found;
    if (match === null) return null;
    const at = (match.index ?? 0) + (match[1] ?? '').length;
    return {sigil: match[2], word: match[3], at, end: at + match[2].length + match[3].length};
  }

  // A finished word becomes the name it stands for, where the reader wrote it: the tokens are
  // taken in whatever order they come, and the words around them are never moved.
  function readWord(completed = false) {
    const word = finishedWord(completed);
    if (word === null) return;
    const token = tokenOf(word.sigil, word.word);
    if (token === null) return; // an unknown word is the reader's own text
    tasks.replace(word.at, word.end, token);
  }

  // completing a word writes it where the caret is, and reads it as the name it stands for
  function insertAt(word: {start: number, end: number}, text: string) {
    const value = input.value;
    input.value = `${value.slice(0, word.start)}${text}${value.slice(word.end)}`;
    input.setSelectionRange(word.start + text.length, word.start + text.length);
    input.dispatchEvent(new Event('input', {bubbles: true}));
    readWord(true);
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
    show(Array.from(subjects)
      .filter(([subject, named]) => subject.toLowerCase().includes(typed) || named.label.toLowerCase().includes(typed))
      .map(([subject, named]) => ({
        label: `${subjectSigil}${named.label}`,
        hint: named.hint,
        insert: () => insertAt(word, `${subjectSigil}${subject}`),
      })));
  };

  input.addEventListener('input', () => {
    offer();
    readWord();
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
