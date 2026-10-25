// the session list of the agent screen: the kebab menus that open as popovers so the
// scrolling list cannot clip them, and the modal pre-fills the global handler cannot do
import {addDelegatedEventListener} from '../../utils/dom.ts';

// the menu is a popover: it renders in the top layer, so the scrolling list cannot clip it
export function initMoreMenus() {
  for (const toggle of document.querySelectorAll<HTMLElement>('.agent-chat-more')) {
    const menu = toggle.parentElement!.querySelector<HTMLElement>('.agent-chat-more-menu');
    if (!menu) continue;
    toggle.addEventListener('click', () => {
      const box = toggle.getBoundingClientRect();
      menu.style.top = `${box.bottom + 4}px`;
      menu.style.right = `${innerWidth - box.right}px`;
      menu.showPopover();
    });
    // an item opens a modal, so the menu must not stay behind it
    menu.addEventListener('click', (event) => {
      if ((event.target as HTMLElement).closest('.agent-chat-more-item')) menu.hidePopover();
    });
  }
  // a fixed menu would drift away from its row while the list scrolls
  document.addEventListener('scroll', () => {
    for (const menu of document.querySelectorAll<HTMLElement>('.agent-chat-more-menu:popover-open')) {
      menu.hidePopover();
    }
  }, {capture: true});

  const folderModal = document.querySelector<HTMLFormElement>('#agent-chat-folder-form');
  const pickModal = document.querySelector<HTMLFormElement>('#agent-chat-folder-pick-form');
  for (const item of document.querySelectorAll<HTMLElement>('.agent-chat-more-item[data-icon]')) {
    item.addEventListener('click', () => {
      const icon = item.getAttribute('data-icon')!;
      for (const input of folderModal!.querySelectorAll<HTMLInputElement>('input[name="icon"]')) {
        input.checked = input.value === icon;
      }
    }, {capture: true}); // run before the global handler opens the modal
  }
  for (const item of document.querySelectorAll<HTMLElement>('.agent-chat-more-item[data-folder]')) {
    item.addEventListener('click', () => {
      const folder = item.getAttribute('data-folder')!;
      pickModal!.querySelector<HTMLSelectElement>('select[name="folder_id"]')!.value = folder;
    }, {capture: true});
  }
  addDelegatedEventListener(document, 'click', '.agent-chat-more-item', () => {}); // keep the delegated bus alive for the items above
}

// a renamed session shows its new title in the list and in the chat header
export function applyConversationTitle(conversation: {id: number, title: string, summary: string}, elConversationID: string) {
  const header = document.querySelector<HTMLElement>('.agent-chat-head-title');
  if (header && elConversationID === String(conversation.id)) header.textContent = conversation.title;
  const id = String(conversation.id);
  for (const link of document.querySelectorAll<HTMLAnchorElement>('.agent-chat-card-main, a.agent-chat-card')) {
    const href = link.getAttribute('href');
    if (!href || href.split('/').pop() !== id) continue;
    const label = link.querySelector<HTMLElement>('.gt-ellipsis, .agent-chat-card-name');
    // the resident session shows what it worked on instead of its own name
    if (label) label.textContent = conversation.summary || conversation.title;
  }
}
