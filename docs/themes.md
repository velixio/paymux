# Dashboard themes

A theme sets the colours of the dashboard. Paymux ships three (**Stripe**, **Paddle** and **Midnight**), you can make your own from a few colours, and each person can pick the one they like. Customer pages (the pay page, subscription checkout, invoices) are not changed by a dashboard theme: they keep using the `tokens` of the page theme described in [the dashboard guide](dashboard.md#themes-and-responsive-behavior).

## Who sees which theme

1. The theme a person chose on their own **Account** page, if it still exists.
2. Otherwise the organisation **default**, set on the **Appearance** page by someone who can change settings.
3. Otherwise **Stripe**.

The default is the same `theme` setting that styles customer pages. If it names a theme without dashboard colours (such as `default` or `minimal-dark`), the dashboard shows Stripe. A theme that is deleted on disk, or that no longer loads, is skipped the same way, so nobody is ever left without a working dashboard.

The light and dark switch in the header works with every theme. A theme may start in dark (`scheme: "dark"`), and the person's own choice, once made, always wins.

## What a theme is

A theme is a folder `<data>/themes/<name>/` with a `theme.json`. Dashboard colours live in its `dashboard` block:

```json
{
  "name": "harbour",
  "version": "1.0.0",
  "description": "Warm paper with a brick-red accent.",
  "dashboard": {
    "seeds": {
      "accent": "#b3261e",
      "background": "#f5f5f2",
      "sidebar": "#121417",
      "text": "#16181d",
      "sidebar_text": "#e8e8e3"
    },
    "dark_seeds": { "accent": "#ff8a80", "background": "#14161a" },
    "scheme": "",
    "overrides": { "light": { "--border": "#d9d9d2" } }
  }
}
```

| Field | Meaning |
|---|---|
| `seeds.accent` | Required. Buttons, links, charts and the selected item. |
| `seeds.background` | The page. Default white. |
| `seeds.sidebar` | The sidebar, title bar and status bar. It may be dark on a light page (Paddle does this). |
| `seeds.text` | Page text. |
| `seeds.sidebar_text` | Text on the sidebar. |
| `dark_seeds` | The same fields for dark mode. Any you leave out are worked out from the light ones, with lightness inverted and the hue kept. |
| `scheme` | `""` (follow the device), `light` or `dark`: the look a person gets before they have chosen. |
| `overrides` | Advanced: set single tokens by hand, for `light` and `dark`. Only dashboard colour tokens are accepted (no `--status-*`), and values are `#rrggbb`. |

All colours are `#rrggbb`. Everything else, such as hover and selected states, borders, muted text and the focus ring, is **derived** from the seeds in the browser.

### Readability is enforced

Derived text colours are adjusted, a step at a time toward black or white, until each pair reaches a contrast ratio of **4.5:1**: text on the page, muted text, sidebar text on the sidebar, highlighted text on its tint, and button text on a button. The button text turns dark on a very light accent (yellow, for example). Values you set in `overrides` are used as written and are not adjusted, so check them in the editor.

**Status colours** (paid, failed, pending and so on) are the same in every theme, so a status means the same thing everywhere.

## The Appearance page

Everything below needs permission to change settings. People who can only read settings see the cards and can export a theme.

- **Cards** show each theme in light and dark, whether it is **Built-in** or **Custom**, which one is the **Default**, and how many people chose it. **Set as default** changes the organisation default.
- **New theme** opens a form with colour pickers; **Duplicate** does the same, starting from another theme's seeds. A duplicate keeps the source's customer-page settings (`tokens`, `font_stack`, stylesheets and assets) but not its `overrides`.
- **Edit** (custom themes only) changes the version, description, seeds and scheme. Whatever the form does not edit, including `overrides`, is kept. While you edit, the whole dashboard shows the theme as you build it, in light or dark, and a table lists the contrast of each text pair with a tick or cross. Saving is switched off while any pair would be hard to read. Leaving the page, or switching to another tab, puts the dashboard back as it was, and the browser asks before you leave with unsaved changes. Without JavaScript the form still works; it just has no live preview.
- **Rename** moves the theme and updates the organisation default and everyone's own choice that named it. It is refused while an app uses the theme for its customer pages (the apps are listed).
- **Delete** is refused while the theme is the default, someone chose it, or an app uses it. The message lists who.
- **Export** downloads `theme.json`. **Import** takes a `theme.json` of up to 64 KB. Only that file is read: stylesheets, templates and images are not imported. If the name is taken, or is a built-in theme's, the form asks for a new name.

Built-in themes (Stripe, Paddle, Midnight, `default` and `minimal-dark`) cannot be changed, replaced or shadowed. Duplicate one to start your own. Every write here asks for a recent sign-in and is recorded in the audit log as `theme.create`, `theme.update`, `theme.rename`, `theme.delete` or `theme.import`; a person changing their own theme is `user.theme`.

## Files on disk

Themes are plain files, so they can also be managed outside the dashboard:

- `<data>/themes/<name>/theme.json`: the manifest. The name inside must match the folder. Names are 1 to 40 lower-case letters, digits or hyphens.
- Optional `css/*.css`, `templates/…` and `assets/…`: customer-page styling, as described in the dashboard guide. The dashboard theme editor never writes these, but duplicating a theme copies them.
- A change to a file is picked up within a second, without a restart. The dashboard reads the colours from `/theme/<name>/dashboard.json`, which any signed-in page can fetch from the same origin.

The editor writes `theme.json` atomically (a temporary file renamed into place), so a reader never sees half a file, and when two admins save the same theme the last save wins.

A theme that fails validation (a bad colour, an unknown field, a value that could break out of a style rule) is listed on the Appearance page with the reason and is not used.
