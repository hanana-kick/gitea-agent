// The tasks a repository agent knows, written once and reached two ways: the cards above the
// composer, and the commands the composer itself understands ("/pr", "/issue"). Either way the
// chosen task lands in the composer as a task, so sending stays the reader's call.
import type {AgentTask} from './task-tokens.ts';

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

// what a task runs on: the subject a card is aimed at, read as the label it stands for
type AgentTaskHost = {set: (task: AgentTask) => void};

function taskOf(preset: AgentPreset, subject: string, label: string, task: string): AgentTask {
  return {command: preset.command, subject, label, task};
}

const commandPrefix = '/';

// one row of the palette the reader walks through
type AgentPresetEntry = {label: string, hint: string, apply: () => void};

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

// the subject a card runs on when the reader picks none: the repository itself
function repositoryLabel(subject: string) {
  return subject;
}

// initPresets wires the cards and the "/" palette of one composer. Its keyDown reports the
// keys the open palette took, so the composer's Enter-to-send leaves them alone.
export function initPresets(form: HTMLFormElement, input: HTMLTextAreaElement, tasks: AgentTaskHost) {
  const presets = readPresets(form);
  const paletteEl = form.querySelector<HTMLElement>('.agent-chat-commands');
  if (presets.length === 0 || !paletteEl) return {keyDown: (_event: KeyboardEvent) => false};
  const palette = paletteEl; // the palette is there from here on

  // the cards: pick a subject, then hand that task to the composer
  for (const card of form.closest('.agent-chat-main')!.querySelectorAll<HTMLElement>('.agent-chat-preset')) {
    const run = card.querySelector<HTMLButtonElement>('.agent-chat-preset-run');
    if (!run) continue;
    run.addEventListener('click', () => {
      const option = card.querySelector<HTMLOptionElement>('.agent-chat-preset-target option:checked');
      const preset = presets.find((p) => p.command === card.getAttribute('data-command'));
      if (!preset) return;
      const subject = option?.getAttribute('data-subject') ?? card.getAttribute('data-subject')!;
      const label = option?.textContent?.trim() ?? repositoryLabel(card.getAttribute('data-subject')!);
      tasks.set(taskOf(preset, subject, label, option?.getAttribute('data-task') ?? card.getAttribute('data-task')!));
      close();
    });
  }

  let entries: AgentPresetEntry[] = [];
  let active = 0;

  // running a task from the palette: a task that takes a subject names itself first
  function runTask(preset: AgentPreset) {
    if (preset.targets.length === 0) {
      tasks.set(taskOf(preset, preset.subject, repositoryLabel(preset.subject), preset.task));
      return;
    }
    input.value = `${commandPrefix}${preset.command} `;
    input.dispatchEvent(new Event('input', {bubbles: true}));
  }

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
        entry.apply();
        offer(); // the chosen row may narrow the list further
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

  function show(next: AgentPresetEntry[]) {
    entries = next;
    active = 0;
    paint();
  }

  // what the reader typed decides what is offered: the commands themselves, or the subjects
  // of the command already named
  function offer() {
    const value = input.value;
    if (!value.startsWith(commandPrefix)) {
      close();
      return;
    }
    const body = value.slice(commandPrefix.length);
    const split = body.indexOf(' ');
    const typed = (split === -1 ? body : body.slice(0, split)).toLowerCase();
    const keyword = (split === -1 ? '' : body.slice(split + 1).trim()).replace(/^@/, '').toLowerCase();
    const preset = split === -1 ? undefined : presets.find((p) => p.command === typed);

    if (preset) {
      if (preset.targets.length === 0) {
        close(); // the task takes no subject: whatever follows is the reader's own words
        return;
      }
      show(preset.targets
        .filter((target) => target.label.toLowerCase().includes(keyword))
        .map((target) => ({
          label: `@${target.label}`,
          hint: preset.title,
          apply: () => tasks.set(taskOf(preset, target.subject, target.label, target.task)),
        })));
      return;
    }

    const commands = presets
      .filter((p) => split === -1 && p.command.startsWith(typed))
      .map((p) => ({label: `${commandPrefix}${p.command}`, hint: p.title, apply: () => runTask(p)}));
    show(commands);
  }

  input.addEventListener('input', offer);

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
        case 'Tab': {
          const entry = entries[active];
          entry.apply();
          offer();
          return true;
        }
        case 'Escape':
          close();
          return true;
      }
      return false;
    },
  };
}
