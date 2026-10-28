---
name: multica-icons
description: Use when a Multica web/desktop UI change needs an icon that `@multica/ui/icons` does not export yet, or when redrawing or reviewing a Multica glyph. Covers the grid, corner and stroke construction, the body/detail markers, and the generate-and-preview loop. Never reach for an icon library, inline SVG, or emoji instead.
---

# Drawing Multica icons

Every icon in the product is a Multica drawing exported from `@multica/ui/icons`. No icon library is installed, and lint rejects imports from one. When you need a new symbol, draw it.

## Before drawing

1. Search `packages/ui/icons/index.ts` for the name and for synonyms. If an existing glyph already means the same thing, add an alias to `ALIASES` in `drawings.mjs` instead of drawing a near-duplicate.
2. Pick a name that describes the symbol, not the feature (`Webhook`, not `IntegrationsNav`). Follow the existing PascalCase names.

## Where things live

- `packages/ui/icons/drawings/construction.mjs` holds the shared geometry helpers.
- `packages/ui/icons/drawings/drawings.mjs` holds every drawing as inner SVG markup, plus `ALIASES`.
- `packages/ui/icons/glyphs.ts` and `index.ts` are generated. Never edit them by hand; `icons.test.ts` fails when they drift.

## Construction

- **Grid.** 24×24 viewBox, with a 2px round-capped, round-joined stroke from the renderer. Keep geometry inside 2.5–21.5. Most silhouettes span 2.75–21.25.
- **Corners.** Use `shape([[x, y, k], ...])` and `rect(x, y, w, h, k)`, which build continuous-curvature (squircle) corners. Typical `k` values:
  - 5–7 for large containers such as cards, windows and chat bubbles.
  - 2.5–4 for small parts.
  - 1–1.5 for tips and points.
  - 0 for a hard corner, which is rare.

  Do not hand-write `a r r` arc corners for rectangles.
- **Arrowheads and chevrons.** Use `head(x, y, dir, depth, half)` for a soft arrowhead with its tip at (x, y). For chevrons, copy the existing `M… s…` soft curves. Never use sharp polyline arrowheads.
- **Dots.** `dot(x, y, r)` with r 1.1–1.5. Use dots only where they carry meaning: info, alert, eyes, status lights.
- **Polygons and stars.** `polygon(cx, cy, radii, count, k)`.
- **Reuse parts.** `file()`, `folderBody`, `bubble`, `ring`, `user()`, `link`, `cw` and `ccw` already exist. A variant (`…Off`, `…Plus`, `…X`) is the base drawing plus one stroke. A slash runs `m3 3 18 18`.

## Body and details

- Mark the main closed silhouette with `class="b"` (body). The active sidebar item fills it solid.
  - Any icon that can appear in navigation must have a body. A test enforces this for route icons.
  - Only mark closed shapes. An open arc with a fill closes into a stray chord.
- Mark every stroke or dot that sits inside the body with `class="k"` (detail). The solid state knocks details out in the background colour; an unmarked detail disappears into the fill.
- **Detail budget.** At most two inner details. Every detail must carry meaning: a check, a symbol, a divider that defines the object. Drop decorative highlights, extra lines and texture. Icons render at 14px in dense rows, where a third detail turns into noise.

## Loop

1. Add the drawing to `DRAWINGS` in `drawings.mjs`. Keep sections grouped by subject.
2. Run `pnpm --filter @multica/ui icons --preview NewName Neighbour1 Neighbour2`. Open the printed HTML file in a browser and check four things:
   - At 48px: does the geometry sit on the grid, with even optical weight next to its neighbours?
   - At 18px and 14px: is it still legible, or do inner details merge?
   - In the solid (active) tile: does the silhouette fill, with every inner detail still visible?
   - Against similar glyphs: can it be told apart at a glance?
3. Run `pnpm --filter @multica/ui icons` to regenerate `glyphs.ts` and `index.ts`.
4. Import the new name from `@multica/ui/icons`. Then run `pnpm --filter @multica/ui test` and the typecheck for the package that uses it.
