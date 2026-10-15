# M2-10 — Command palette, keyboard navigation, toasts with undo

- Command palette (Ctrl/⌘+K or the header button): fuzzy-free
  substring search over every page (derived from the page
  registry, so pages added by other units appear automatically)
  plus quick actions (refresh, create provider/route/key,
  toggle theme, drain toggle, open /metrics). Arrow keys +
  Enter, Esc closes, listbox semantics.
- Keyboard navigation: Alt+1..N jumps between pages in
  registry order; "?" opens a shortcuts help dialog. Single-key
  shortcuts never fire while typing in a field.
- Toasts with undo for destructive actions: revoking a virtual
  key and deleting a route now commit through a 5-second
  delayed action whose toast carries an Undo button; undoing
  leaves the server untouched (the API call never fires).
  Only one pending action at a time — scheduling a second one
  commits the first immediately rather than dropping it.
