---
name: ui-ux-craft
description: Production-grade UI/UX — anti-AI-slop rules, design tokens, accessibility, pre-delivery checklist. Load SKILL.md + on-demand modules from references/.
version: 1
---

# UI/UX Craft

Use for ANY task that produces visual UI. Skip for pure backend/API/CLI.

---

## Scope Detector (run first)

| Task Type | Example | Modules to Load |
|---|---|---|
| **Tweak** | Change button text, adjust color, fix spacing | Core only (this file) |
| **Component** | New button, card, form, modal | Core + `references/components.md` |
| **Page** | New page, full redesign, landing page | Core + all `references/*.md` |
| **Review** | Audit existing UI for quality | Core + `references/checklist.md` |

---

## 🎯 Core Philosophy

> Three generic AI defaults you MUST avoid:
> 1. Warm cream + serif + terracotta (#F4F1EA bg)
> 2. Dark bg + acid green/vermilion accent
> 3. Broadsheet newspaper (hairline rules, zero border-radius)
>
> These are legitimate for SOME briefs, but they are defaults, not choices.

**Signature Element:** Every page has ONE memorable thing. Spend boldness in ONE place. Keep everything else quiet.

---

## 🔴 MUST Rules (blocks delivery)

| # | Rule | Why |
|---|---|---|
| M1 | Body text ≥ 16px (0.875rem minimum for captions) | Readability |
| M2 | Color contrast ≥ 4.5:1 (WCAG AA) | Accessibility |
| M3 | No horizontal scroll at any breakpoint | Mobile breakage |
| M4 | Touch targets ≥ 44×44px on mobile | Usability |
| M5 | All images have `alt` text | Accessibility |
| M6 | All form inputs have visible `<label>` | Accessibility |
| M7 | Visible focus indicator on all interactive elements | Keyboard nav |
| M8 | Define token system before writing code | Consistency |

---

## 🟡 SHOULD Rules (best practice)

| # | Rule | Why |
|---|---|---|
| S1 | Maximum 2 font families (3 if mono needed) | Visual coherence |
| S2 | Use spacing scale values only (4/8/12/16/24/32/48/64px) | Consistency |
| S3 | Never use pure #000 for text or #FFF for backgrounds | Polish |
| S4 | Active voice: "Save changes" not "Submit" | Clarity |
| S5 | Sentence case: "Settings page" not "Settings Page" | Consistency |
| S6 | Mobile-first: base styles for mobile, add complexity at lg+ | Responsive |
| S7 | Reduced motion: `@media (prefers-reduced-motion: reduce)` | Accessibility |
| S8 | Escape closes modals/dropdowns | UX |
| S9 | Loading states for all async actions | UX |
| S10 | Error states with clear recovery path | UX |

---

## 🟢 Nice-to-Have (polish)

| # | Rule |
|---|---|
| N1 | Stagger animations by 50-100ms for lists |
| N2 | Enter faster than exit (~200ms vs ~150ms) |
| N3 | Easing: `cubic-bezier(0.4, 0, 0.2, 1)` for most UI |
| N4 | 60-30-10 color rule (surface/text/accent) |
| N5 | Empty states with call-to-action |
| N6 | Hover + focus + active + disabled states on interactive elements |

---

## 🚨 Anti-Slop Quick Check

Before delivering, scan for these. If ANY match → fix:

| Pattern | Symptom |
|---|---|
| Dashboard gradient | Gratuitous gradient on cards/headers |
| Padding explosion | Inconsistent padding (12px, 16px, 24px mixed) |
| Center everything | All text centered, no visual hierarchy |
| Icon soup | Emoji/icon on every element |
| Card-itis | Everything wrapped in cards |
| Font salad | 3+ font families |
| Hover-only | Functionality only on hover (breaks mobile) |
| No focus | Missing keyboard focus indicators |
| Placeholder labels | Using placeholder as only label in forms |

---

## 📐 Token System (MANDATORY for Component/Page tasks)

Define before writing ANY code:

```
Colors (4-6 with names):   --color-primary, --color-surface, --color-text, --color-accent
Typography (2-3 roles):    --font-display, --font-body, --font-mono (if needed)
Spacing (from scale):      4/8/12/16/24/32/48/64px
Radius:                    sm(4) md(8) lg(12) xl(16) full(9999)
```

Every value must have a NAME and PURPOSE. No orphan hex values.

---

## 📚 On-Demand Modules

Load these via `read_file` when scope requires:

| File | When to Load |
|---|---|
| `references/a11y.md` | Full accessibility audit, keyboard nav, ARIA |
| `references/animation.md` | Complex animations, GSAP, scroll effects |
| `references/stacks.md` | Stack-specific rules (React, Vue, Tailwind, Flutter, SwiftUI) |
| `references/components.md` | Building buttons, forms, cards, nav, modals |
| `references/checklist.md` | Full pre-delivery checklist (20+ items) |

---

## 🧠 Process

1. **Scope** → Run scope detector above
2. **Tokens** → Define color/typography/spacing/radius
3. **Build** → Follow MUST rules, apply SHOULD where possible
4. **Self-check** → Scan anti-slop list
5. **Deliver** → Pass MUST rules (checklist blocks delivery)

---

## Anti-Pattern → Fix Lookup

| Anti-Pattern | Fix |
|---|---|
| Dashboard gradient | Remove or make it encode status/info |
| Padding explosion | Pick 2-3 spacing values from scale, use consistently |
| Center everything | Left-align body text, center only headlines/hero |
| Font salad | Reduce to 2 families (display + body) |
| Hover-only | Add visible state + touch-friendly hit area |
| Placeholder labels | Add `<label>` above input, keep placeholder as hint |
| No focus | Add `:focus-visible` ring with 2px offset |
| Card-itis | Use sections/dividers instead of wrapping in cards |
| Icon soup | Remove icons from non-interactive elements |
| Tiny text | Bump to 16px body, 12px minimum for captions |
