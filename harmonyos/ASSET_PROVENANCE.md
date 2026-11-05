# Ollama desktop assets

Source repository: the pinned upstream Ollama checkout used by
`harmonyos/tools/prepare_official_app.py`, commit
`8a971df3bacec93944ec894f2939fd2567afc5dd` (MIT license).

- APP/title-bar/dock icon: `app/assets/app.ico`. The pinned ICO contains one
  256×256 PNG frame. `scripts/prepare_harmonyos_app_icon.py` extracts that frame
  byte-for-byte into `harmonyos/hap/AppScope/resources/base/media/ollama_icon.png`.
  It does not redraw, rasterize, recolor, or use an AI-generated logo.
- Tray glyphs: the three paths in upstream `app/ui/app/src/components/Logo.tsx`,
  retaining their viewBox geometry, with black/white fills for the native status
  bar's respective light/dark backgrounds. Files are
  `harmonyos/hap/entry/src/main/resources/rawfile/tray-{black,white}.svg`.

The SVG files begin with `<svg>` for image-decoder compatibility; provenance is
kept here instead of prepending XML comments. A first SVG APP-icon attempt did
not render correctly on the emulator and was replaced with the original PNG.
That observation alone does not establish which SVG feature caused the failure.

These assets retain the upstream project's licensing and branding boundaries;
the debug HarmonyOS adaptation is not an official Ollama release.

The extracted PNG and tray SVG sources are included directly; normal builds do not need the historical extraction script or private workspace.
