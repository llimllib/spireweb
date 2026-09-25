// Client-side behavior.
//
// Deliberately small. HTMX drives search and lazy tool expansion, <details>
// handles collapsing, and navigation is plain links, so the only things left
// for JavaScript are the ones the platform genuinely lacks: keyboard
// navigation, scroll restoration, and the clipboard.

const listPane = () => document.querySelector<HTMLElement>(".list");
const rowLinks = () =>
  Array.from(document.querySelectorAll<HTMLAnchorElement>("a.row"));

/**
 * Moving the cursor means moving DOM focus to the row's anchor.
 *
 * The alternative -- tracking a "cursor" class ourselves -- means
 * reimplementing scroll-into-view, Enter-to-activate, and focus visibility,
 * and leaves the keyboard cursor invisible to assistive technology. Focus
 * gives all of that from the platform.
 */
function focusRow(index: number): void {
  const rows = rowLinks();
  if (rows.length === 0) return;

  const clamped = Math.max(0, Math.min(index, rows.length - 1));
  const target = rows[clamped];
  if (!target) return;

  target.focus({ preventScroll: true });
  target.scrollIntoView({ block: "nearest" });
}

/**
 * Where the cursor is now: the focused row if one has focus, otherwise the
 * selected one, so the first `j` continues from what is on screen rather than
 * jumping to the top of the list.
 */
function currentIndex(): number {
  const rows = rowLinks();
  const focused = rows.findIndex((r) => r === document.activeElement);
  if (focused !== -1) return focused;
  const selected = rows.findIndex((r) => r.matches('[aria-current="page"]'));
  return selected === -1 ? -1 : selected;
}

function moveCursor(delta: number): void {
  const from = currentIndex();
  // From nothing, j goes to the first row and k to the last, which is what
  // both vim and every mail client do.
  if (from === -1) {
    focusRow(delta > 0 ? 0 : rowLinks().length - 1);
    return;
  }
  focusRow(from + delta);
}

const searchBox = () =>
  document.querySelector<HTMLInputElement>('input[name="q"]');

function focusSearch(): void {
  const box = searchBox();
  if (!box || box.disabled) return;
  box.focus();
  box.select();
}

/** True when the user is typing, and keystrokes are theirs rather than ours. */
function isTyping(target: EventTarget | null): boolean {
  if (!(target instanceof HTMLElement)) return false;
  return (
    target instanceof HTMLInputElement ||
    target instanceof HTMLTextAreaElement ||
    target.isContentEditable
  );
}

// ---------------------------------------------------------------- help sheet

function helpDialog(): HTMLDialogElement | null {
  return document.querySelector<HTMLDialogElement>("#help");
}

function toggleHelp(): void {
  const dialog = helpDialog();
  if (!dialog) return;
  if (dialog.open) dialog.close();
  else dialog.showModal();
}

// ------------------------------------------------------------ scroll restore

/**
 * Navigation is a full page load, so the list pane would otherwise jump back
 * to the top every time a session is opened.
 *
 * Keyed by the query, because the list holds different rows when filtered and
 * restoring a scroll position across that is meaningless.
 */
function scrollKey(): string {
  const q = new URLSearchParams(location.search).get("q") ?? "";
  return `spireweb:list-scroll:${q}`;
}

function saveScroll(): void {
  const pane = listPane();
  if (!pane) return;
  try {
    sessionStorage.setItem(scrollKey(), String(pane.scrollTop));
  } catch {
    // Private browsing, or a full quota. Losing a scroll position is not
    // worth an error.
  }
}

function restoreScroll(): void {
  const pane = listPane();
  if (!pane) return;
  let saved: string | null = null;
  try {
    saved = sessionStorage.getItem(scrollKey());
  } catch {
    return;
  }
  if (saved === null) return;

  const top = Number(saved);
  if (Number.isFinite(top)) pane.scrollTop = top;
}

// --------------------------------------------------------------- match scroll

/**
 * Opening a search result lands at the top of a session that may be hundreds
 * of messages long, so the server names the message that matched and we bring
 * it into view.
 *
 * Nothing happens when the attribute is absent, which is the browsing case and
 * the case where a match could not be located.
 */
function scrollToMatch(): void {
  const pane = document.querySelector<HTMLElement>(".reading");
  const id = pane?.dataset.scrollTo;
  if (!pane || !id) return;

  // getElementById rather than querySelector(`#${id}`), which would treat the
  // value as a selector and throw on anything that is not a valid one.
  const target = document.getElementById(id);
  if (!target) return;

  // Centre a match that fits, so it reads with the message before it. A match
  // taller than the pane cannot be centred -- that puts its first line above
  // the fold and lands the reader mid-sentence -- so show its top, nudged down
  // far enough to be visibly not the start of the session.
  const fits = target.getBoundingClientRect().height < pane.clientHeight;
  target.scrollIntoView({ block: fits ? "center" : "start" });
  if (!fits) pane.scrollBy(0, -16);
}

// ------------------------------------------------------------ copy markdown

/**
 * Copies a turn's markdown source, which the server embeds beside the
 * rendered prose in a <template>. Selecting the prose and copying gives the
 * rendered text, with code fences, links and emphasis flattened out of it.
 */
async function copyTurn(button: HTMLButtonElement): Promise<void> {
  const source = button
    .closest(".turn")
    ?.querySelector<HTMLTemplateElement>("template.turn-source");
  if (!source) return;

  let ok = true;
  try {
    await writeClipboard(source.content.textContent ?? "");
  } catch {
    ok = false;
  }
  flash(button, ok ? "is-copied" : "is-failed", ok ? "Copied" : "Copy failed");
}

/**
 * navigator.clipboard exists only in a secure context. 127.0.0.1 is one;
 * `--addr 0.0.0.0:8080` reached by LAN address is not, and there the
 * deprecated execCommand is the only way left.
 */
async function writeClipboard(text: string): Promise<void> {
  if (navigator.clipboard) {
    await navigator.clipboard.writeText(text);
    return;
  }
  const area = document.createElement("textarea");
  area.value = text;
  area.setAttribute("readonly", "");
  area.style.position = "fixed";
  area.style.opacity = "0";
  document.body.append(area);
  area.select();
  const copied = document.execCommand("copy");
  area.remove();
  if (!copied) throw new Error("execCommand copy refused");
}

const flashTimers = new WeakMap<HTMLElement, number>();

/** Shows a result on the button for a moment, and tells a screen reader. */
function flash(button: HTMLButtonElement, cls: string, label: string): void {
  const original = button.title;
  button.classList.remove("is-copied", "is-failed");
  button.classList.add(cls);
  button.setAttribute("aria-label", label);

  window.clearTimeout(flashTimers.get(button));
  flashTimers.set(
    button,
    window.setTimeout(() => {
      button.classList.remove(cls);
      button.setAttribute("aria-label", original);
    }, 1500),
  );
}

// ------------------------------------------------------------------ bindings

// gg, as in vim: g is a prefix, and only a second g within the timeout acts.
let pendingG = false;
let pendingGTimer: number | undefined;

function noteG(): void {
  pendingG = true;
  window.clearTimeout(pendingGTimer);
  pendingGTimer = window.setTimeout(() => {
    pendingG = false;
  }, 600);
}

function onKeydown(event: KeyboardEvent): void {
  // Cmd-K reaches the search box from anywhere, including the search box.
  if ((event.metaKey || event.ctrlKey) && event.key.toLowerCase() === "k") {
    event.preventDefault();
    focusSearch();
    return;
  }

  if (event.key === "Escape") {
    const box = searchBox();
    if (document.activeElement === box && box) {
      box.blur();
      focusRow(Math.max(currentIndex(), 0));
      event.preventDefault();
    } else if (helpDialog()?.open) {
      helpDialog()?.close();
    }
    return;
  }

  // Everything below is a bare key, so it must not fire while typing or
  // while a modifier is held.
  if (isTyping(event.target) || event.metaKey || event.ctrlKey || event.altKey) {
    return;
  }

  const wasPendingG = pendingG;
  if (event.key !== "g") pendingG = false;

  switch (event.key) {
    case "j":
    case "ArrowDown":
      event.preventDefault();
      moveCursor(1);
      break;

    case "k":
    case "ArrowUp":
      event.preventDefault();
      moveCursor(-1);
      break;

    case "g":
      event.preventDefault();
      if (wasPendingG) {
        pendingG = false;
        focusRow(0);
      } else {
        noteG();
      }
      break;

    case "G":
      event.preventDefault();
      focusRow(rowLinks().length - 1);
      break;

    case "/":
      event.preventDefault();
      focusSearch();
      break;

    case "?":
      event.preventDefault();
      toggleHelp();
      break;

    // Enter needs no handler: the cursor is real focus on a real link.
  }
}

function init(): void {
  restoreScroll();
  scrollToMatch();

  document.addEventListener("keydown", onKeydown);

  document
    .querySelector<HTMLButtonElement>("#help-open")
    ?.addEventListener("click", toggleHelp);

  // Delegated, so one listener covers every turn in however long a session.
  document.addEventListener("click", (event) => {
    const target = event.target;
    if (!(target instanceof Element)) return;
    const button = target.closest<HTMLButtonElement>("button.turn-copy");
    if (button) void copyTurn(button);
  });

  // pagehide rather than unload: it fires for back/forward cache navigations
  // too, which unload does not.
  window.addEventListener("pagehide", saveScroll);

  // New results are a new list, so start at the top rather than restoring a
  // position that referred to different rows.
  document.body.addEventListener("htmx:afterSwap", (event) => {
    const target = (event as CustomEvent).target;
    if (target instanceof HTMLElement && target.id === "rows") {
      listPane()?.scrollTo({ top: 0 });
    }
  });
}

if (document.readyState === "loading") {
  document.addEventListener("DOMContentLoaded", init);
} else {
  init();
}
