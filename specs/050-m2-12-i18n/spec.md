# M2-12 — English/Persian/Pashto i18n, RTL, persisted switcher

Scope of this unit (stated plainly): the application CHROME is
localized — primary navigation, page titles and descriptions
for every page, the brand line, skip link, connection/live
status, sign-in controls, and the theme toggle — in English,
Persian (fa) and Pashto (ps), with full RTL direction switching
for fa/ps (shell layout mirrored via [dir=rtl] rules) and the
choice persisted in localStorage (nexaroute_lang). Page BODY
copy (data-driven operator content inside the views) remains
English in this unit; the dictionary structure (t() + per-page
meta tables) is the seam for extending coverage view by view.

RTL screenshots are part of M2-14's browser evidence and share
its environment boundary: the sandbox Chromium build blocks
loopback navigation (see docs/browser-evidence/m2-11/), so
screenshots are UNVERIFIED in this environment.
