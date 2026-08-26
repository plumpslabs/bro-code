# Pre-Delivery Checklist

Full quality gate. Run before saying "done" on Component/Page tasks.

---

## Visual (MUST pass)

| # | Check |
|---|---|
| V1 | Token system defined and used consistently |
| V2 | No orphan hex colors (every color has name/purpose) |
| V3 | No random pixel values (all spacing from scale) |
| V4 | Typography hierarchy clear (display → heading → body → caption) |
| V5 | No anti-slop patterns detected |

## Responsive (MUST pass)

| # | Check |
|---|---|
| R1 | Works at 375px (iPhone SE), 768px (iPad), 1280px (desktop) |
| R2 | No horizontal scroll at any breakpoint |
| R3 | Touch targets ≥ 44px on mobile |
| R4 | Text readable without zoom on mobile |

## Accessibility (MUST pass)

| # | Check |
|---|---|
| A1 | All images have alt text |
| A2 | All form inputs have labels |
| A3 | Color contrast ≥ 4.5:1 |
| A4 | Keyboard navigable (tab, enter, escape, arrows) |
| A5 | Focus visible on all interactive elements |
| A6 | Reduced motion supported |

## Interaction (SHOULD pass)

| # | Check |
|---|---|
| I1 | Loading states for async actions |
| I2 | Error states with clear recovery path |
| I3 | Empty states with call-to-action |
| I4 | Hover + focus + active + disabled states on interactive elements |

## Code Quality (SHOULD pass)

| # | Check |
|---|---|
| C1 | CSS uses custom properties (not hardcoded values) |
| C2 | No inline styles (except dynamic values) |
| C3 | Consistent naming (BEM, utility-first, or component-scoped) |
| C4 | Semantic HTML (button, nav, main, section — not div-everything) |
