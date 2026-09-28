# Web UI consistency

Read [STYLE.md](STYLE.md) before changing this interface. It defines the shared
visual patterns; `static/app.css` implements them. Reuse its tokens, button variants,
action groups and dialog structure. Do not add per-button ID overrides for
typography, spacing, geometry or color. Apply the same classes to controls created
in `static/app.js` as to controls declared in `static/index.html`.

Verify changed UI at desktop and mobile widths. Keep behavior and existing user
work intact. A style change alone does not authorize committing or pushing.
