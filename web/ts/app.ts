// Client-side behavior.
//
// Deliberately small. HTMX drives search and lazy tool expansion, <details>
// handles collapsing, and navigation is plain links, so the only things that
// need JavaScript are the ones the platform genuinely lacks: keyboard
// navigation, scroll restoration, and a focus shortcut. Those arrive with the
// Polish milestone; this file exists now so the toolchain is wired up.

export {};
