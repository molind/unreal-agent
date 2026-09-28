# Web chat visual patterns

`static/app.css` is the source of truth for this interface. Keep the existing dark
green palette, system typeface, quiet surfaces and restrained contrast. Extend
shared rules when adding a feature; do not add per-control style exceptions.

## Language and terminology

The interface language is Belarusian (`lang="be"`): labels, placeholders,
accessible names, connection states, empty states and dynamic controls must all
use it. Use **праект** for the registered workspace and **размова** for a session;
**папка** describes the physical directory. Format dates and plural forms with
locale `be`. Commands, paths, user/assistant content and technical diagnostics
remain verbatim. Common application errors are localized; unknown diagnostics
have a Belarusian explanation rather than being silently discarded.

## Tokens and controls

Use `:root` variables for shared colors, spacing and dimensions. Spacing uses a
4 px step. Buttons and fields share `--radius-control`; dialogs use
`--radius-panel`. Standard buttons have a 36 px minimum height, 13 px text,
500 weight, 1.4 line height and 8 × 12 px padding. Mobile targets grow to at least
44 px without shrinking text. Labels may wrap and grow taller. Supporting text
uses `--font-small` (12 px), including on mobile.

Every action uses a native `<button>` and the common button geometry:

| Pattern | Class | Use |
| --- | --- | --- |
| Normal | none | Secondary action, such as Новая размова |
| Primary | `primary` | Main form/decision action, such as Даслаць |
| Quiet | `quiet` | Toolbar, navigation or Скасаваць |
| Destructive | `danger` | Irreversible action; `quiet danger` in the action popover |
| Icon | `icon` | Square quiet control with a Belarusian accessible name |
| Compact | `compact` | Dense desktop utility, such as Капіяваць; 44 px on mobile |

Do not set button font size, padding, height, radius or colors by ID or container.
Variants only change emphasis. `icon` and `compact` are the size exceptions.
Navigation rows may align left and have multi-line titles, but use common control
padding and text size. Pin controls use `icon`.

Hover applies only to enabled controls. Keyboard focus uses a visible outline,
including the message field. Disabled controls use common opacity and cursor.
Pending actions disable duplicate submission and show a Belarusian progress label.
Connection text and indicator color must agree; green means connected, not retrying.

## Actions and navigation

Use `actions` for aligned, wrapping groups with the shared gap. Cancel/decline
comes first; the committing action comes last. Layout classes must not restyle
buttons. On mobile, the composer hint gets a separate row, not smaller text.

The conversation header contains title/status and a single `⋯` disclosure button.
`conversation-actions` anchors the vertically stacked `action-popover`: first
**Перайменаваць…**, then **Выдаліць размову…**. Opening it focuses the first action.
Renaming uses the shared dialog/form styles, prefills and selects the current
name, and offers **Скасаваць** / **Захаваць**. Keep typed names intact on errors
and live updates; bind submission to the conversation that opened the dialog.
The disclosure has `aria-expanded`/`aria-controls`, closes on Escape, outside click
or focus leaving the group, and returns focus appropriately. It is a disclosure,
not an ARIA menu requiring arrow-key navigation. There is no manual Release button:
idle resource cleanup is automatic and does not delete conversations.

The mobile sidebar marks the main area inert while open, contains keyboard focus,
closes with Escape and restores focus to its opener. Native dialogs manage their
own modal focus even when opened from the sidebar.

## Dialogs and feedback

Use native `<dialog>` with `aria-labelledby`, a form, explanatory paragraphs, an
inline `error` message and `actions dialog-actions`. Ordinary confirmation uses
`primary`; destructive confirmation uses `danger`. Focus **Скасаваць** when opening
a destructive dialog. Width, padding, scrolling and backdrop use shared rules.
Keep irreversible consequences and the affected conversation visible; secondary
retention details may be in a `<details>` disclosure. Do not imply secure erasure.

Use the common `error` class for all form errors. Keep conversation notices in the
notice area. Use textContent for user-supplied titles and messages.

## Verification

Check normal, hover, focus and disabled states, long labels, desktop and 390/320 px
mobile layouts. Action groups and dialogs must fit without horizontal overflow.
Preserve keyboard access, accessible names, drafts, retries and permission scoping.
Run `node --test cmd/internal/webchat/ui_test.mjs` with Chrome/Chromium installed
(or set `CHROME_BIN` to its executable). The test uses mock APIs and an isolated
browser profile; it does not connect to the running agent server.
