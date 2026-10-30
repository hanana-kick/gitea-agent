// What this agent can be asked to do, written once and reached three ways: the cards above the
// composer, the "/" that calls a skill, and the "@" that names a subject. The two sigils are
// separate — either may stand alone — and a line that carries them is parsed into a task when
// it is completed from the palette or broken by the reader.
import type {AgentToken, ComposerInput} from './input.ts';

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

// what a word of this page stands for: a skill reads by its own name, a subject by what it
// names; where nothing is known — a session with no repository — a subject is its own name
export function presetResolver(form: HTMLFormElement): (word: string) => AgentToken | null {
  const presets = readPresets(form);
  const subjects = new Map<string, string>();
  for (const preset of presets) {
    if (preset.subject !== '') subjects.set(preset.subject, preset.subject);
    for (const target of preset.targets) subjects.set(target.subject, target.label);
  }
  const skills = new Map(presets.map((preset) => [preset.command, preset.title]));
  return (word) => {
    if (word.startsWith(commandSigil)) {
      const command = word.slice(1);
      if (skills.size > 0 && !skills.has(command)) return null;
      return {display: skills.get(command) ?? command, canonical: word};
    }
    if (!word.startsWith(subjectSigil)) return null;
    const subject = word.slice(1);
    if (subjects.size > 0 && !subjects.has(subject)) return null;
    return {display: `${subjectSigil}${subjects.get(subject) ?? subject}`, canonical: word};
  };
}

// the sigil word the caret is in, when what is written before it is one: what the palette completes
function sigilWord(before: string) {
  const match = /(^|\s)([/@])(\S*)$/.exec(before);
  return match ? {sigil: match[2], typed: match[3]} : null;
}

export function initPresets(form: HTMLFormElement, input: ComposerInput) {
  const presets = readPresets(form);
  const paletteEl = form.querySelector<HTMLElement>('.agent-chat-commands');
  if (presets.length === 0 || !paletteEl) return {keyDown: (_event: KeyboardEvent) => false};
  const palette = paletteEl; // the palette is there from here on
  const resolve = presetResolver(form);

  // every subject this agent can be aimed at, named once: the tasks share them
  const subjects = new Map<string, {label: string, hint: string}>();
  for (const preset of presets) {
    if (preset.subject !== '') subjects.set(preset.subject, {label: preset.subject, hint: preset.title});
    for (const target of preset.targets) subjects.set(target.subject, {label: target.label, hint: preset.title});
  }
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
        `${commandSigil}${preset.command}`,
        subject === '' ? null : `${subjectSigil}${subject}`,
      ].filter((word) => word !== null);
      input.setText(`${words.join(' ')}${request === '' ? '' : `\n${request}`}`);
      input.applyTokens(resolve); // the words read as their names, where they were written
      input.focusToEnd();
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
      // the caret must stay in the box, so the palette answers the press itself
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

  // completing a word writes it where the caret is, and the box reads it as its name
  function insert(word: string, typed: number) {
    input.replaceBefore(1 + typed, word);
    readWord(true);
  }

  // A word is read once it is finished: by its own end when the palette completed it, by the
  // space the reader broke it with — and that space keeps its place in the text.
  function readWord(completed: boolean) {
    const before = input.beforeCaret();
    const match = (completed ? /(^|\s)([/@])(\S+)$/ : /(^|\s)([/@])(\S+)(\s+)$/).exec(before);
    if (!match) return;
    const token = resolve(match[2] + match[3]);
    if (!token) return; // an unknown word is the reader's own text
    input.replaceBefore(1 + match[3].length, token, match[4]?.length ?? 0);
  }

  // what the caret is typing decides what is offered: a skill after "/", a subject after "@"
  const offer = () => {
    const word = sigilWord(input.beforeCaret());
    if (!word) {
      close();
      return;
    }
    const typed = word.typed.toLowerCase();
    if (word.sigil === commandSigil) {
      show(presets
        .filter((preset) => preset.command.toLowerCase().startsWith(typed))
        .map((preset) => ({
          label: `${commandSigil}${preset.command}`,
          hint: preset.title,
          insert: () => insert(`${commandSigil}${preset.command}`, word.typed.length),
        })));
      return;
    }
    show(Array.from(subjects)
      .filter(([subject, named]) => subject.toLowerCase().includes(typed) || named.label.toLowerCase().includes(typed))
      .map(([subject, named]) => ({
        label: `${subjectSigil}${named.label}`,
        hint: named.hint,
        insert: () => insert(`${subjectSigil}${subject}`, word.typed.length),
      })));
  };

  input.el.addEventListener('input', () => {
    offer();
    readWord(false);
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
