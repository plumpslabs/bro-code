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

> AI-generated UI looks generic because it guesses. Professional UI looks intentional because it follows a system.

**The Anti-Template Rule:** Every design decision must be TRACEABLE to a token, a pattern, or a reference. If you can't name why you chose that color/spacing/font — it's random, and random looks like a template.

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

## 🏆 Golden Examples — Template vs Pro

These BEFORE/AFTER examples show what separates template UI from professional UI. The examples use React/TSX for brevity, but the **PRINCIPLES are universal** — apply them to HTML/CSS, Blade, Rails, Django, Tailwind, or any stack.

> **Stack-agnostic rule:** The TOKEN SYSTEM (colors, spacing, typography) and the DESIGN PATTERNS (hierarchy, semantic colors, consistent spacing) are the same regardless of framework. Only the syntax changes.

### Example 1: Pricing Page

**❌ TEMPLATE (what AI defaults to — ANY stack):**
```html
<!-- Gradient hero + 3 identical cards + centered everything = TEMPLATE -->
<div style="background: linear-gradient(135deg, #3b82f6, #8b5cf6); color: white; padding: 80px 0; text-align: center;">
  <h1 style="font-size: 2.5rem; font-weight: 700;">Choose Your Plan</h1>
  <p style="margin-top: 16px; color: #bfdbfe;">The perfect plan for every team</p>
</div>
<div style="display: flex; justify-content: center; gap: 32px; padding: 64px 0;">
  <div style="background: white; border-radius: 16px; box-shadow: 0 20px 40px rgba(0,0,0,0.1); padding: 32px; width: 320px;">
    <h3 style="font-size: 1.25rem; font-weight: 700; text-align: center;">Starter</h3>
    <p style="font-size: 2rem; font-weight: 700; text-align: center; margin-top: 16px;">$9</p>
    <button style="width: 100%; margin-top: 24px; background: #3b82f6; color: white; padding: 8px 0; border-radius: 8px; border: none;">
      Get Started
    </button>
  </div>
</div>
```

**✅ PRO (HTML + CSS tokens — works in ANY stack):**
```html
<!-- No gradient hero. Clean hierarchy. Semantic tokens. Intentional spacing. -->
<section style="max-width: 960px; margin: 0 auto; padding: var(--space-16) var(--space-6);">
  <div style="max-width: 640px;">
    <span class="badge badge--secondary">Pricing</span>
    <h2 style="font-size: 1.875rem; font-weight: 600; letter-spacing: -0.025em; color: var(--color-text); margin-top: var(--space-4);">
      Simple, transparent pricing
    </h2>
    <p style="font-size: 1.125rem; color: var(--color-muted); margin-top: var(--space-2);">
      No hidden fees. Cancel anytime.
    </p>
  </div>

  <div style="display: grid; grid-template-columns: repeat(3, 1fr); gap: var(--space-6); margin-top: var(--space-12);">
    <div class="card card--featured">
      <span class="badge badge--primary" style="position: absolute; top: -12px; left: 24px;">Most popular</span>
      <h3 class="card__title">Pro</h3>
      <p class="card__description">For growing teams</p>
      <div style="margin-top: var(--space-6);">
        <span style="font-size: 2rem; font-weight: 700;">$29</span>
        <span style="font-size: 0.875rem; color: var(--color-muted);">/mo</span>
      </div>
      <ul style="margin-top: var(--space-6); space-y: var(--space-3);">
        <li style="display: flex; align-items: center; gap: var(--space-2); font-size: 0.875rem;">
          <svg class="icon--primary" width="16" height="16">✓</svg> Unlimited projects
        </li>
      </ul>
      <button class="btn btn--primary" style="width: 100%; margin-top: var(--space-6);">Get started</button>
    </div>
  </div>
</section>
```

**What changed (universal, any stack):**
- No gradient hero → Badge + heading + description (clean hierarchy)
- Centered everything → left-aligned body, structured layout
- Hardcoded hex → CSS custom properties (`var(--color-primary)`, `var(--space-6)`)
- Random padding → spacing scale values
- No featured indicator → subtle badge for emphasis
- Generic copy → specific, useful description

**Same pattern in Tailwind:**
```html
<section class="max-w-5xl mx-auto px-6 py-16">
  <div class="max-w-2xl">
    <span class="inline-block px-3 py-1 text-xs font-medium rounded-full bg-muted/10 text-muted mb-4">Pricing</span>
    <h2 class="text-3xl font-semibold tracking-tight text-foreground">Simple, transparent pricing</h2>
    <p class="mt-2 text-lg text-muted">No hidden fees. Cancel anytime.</p>
  </div>
</section>
```

**Same pattern in Blade (Laravel):**
```blade
<section class="max-w-5xl mx-auto px-6 py-16">
  <div class="max-w-2xl">
    <x-badge variant="secondary">Pricing</x-badge>
    <h2 class="text-3xl font-semibold tracking-tight text-foreground">Simple, transparent pricing</h2>
    <p class="mt-2 text-lg text-muted">No hidden fees. Cancel anytime.</p>
  </div>
</section>
```

### Example 2: Dashboard Stats

**❌ TEMPLATE:**
```html
<!-- Dark gradient card + all green changes + no hierarchy -->
<div style="display: grid; grid-template-columns: repeat(4, 1fr); gap: 16px;">
  <div style="background: linear-gradient(135deg, #1f2937, #111827); border-radius: 12px; padding: 24px; color: white;">
    <p style="color: #9ca3af; font-size: 0.875rem;">Revenue</p>
    <p style="font-size: 1.5rem; font-weight: 700; margin-top: 8px;">$45,231</p>
    <p style="color: #22c55e; font-size: 0.75rem; margin-top: 4px;">↑ 12.5%</p>
  </div>
</div>
```

**✅ PRO (CSS tokens — works anywhere):**
```html
<div style="display: grid; grid-template-columns: repeat(4, 1fr); gap: var(--space-4);">
  <div class="card" style="padding: var(--space-6);">
    <div style="display: flex; align-items: center; justify-content: space-between; padding-bottom: var(--space-2);">
      <p style="font-size: 0.875rem; font-weight: 500; color: var(--color-muted);">Revenue</p>
      <svg class="icon" width="16" height="16" style="color: var(--color-muted);">$</svg>
    </div>
    <p style="font-size: 1.5rem; font-weight: 700;">$45,231</p>
    <p style="font-size: 0.75rem; color: var(--color-muted); margin-top: var(--space-1);">
      <span style="color: var(--color-success);">+12.5%</span> from last month
    </p>
  </div>
</div>
```

**What changed:**
- Dark gradient → white card + subtle border (token: `var(--color-border)`)
- All green → semantic: green for positive, red for negative
- Icon in header → scannable, not decorative
- "from last month" context → meaningful, not just a number

### Example 3: Login Form

**❌ TEMPLATE:**
```html
<!-- Gradient bg + placeholder-only + no labels -->
<div style="min-height: 100vh; background: linear-gradient(135deg, #6366f1, #8b5cf6); display: flex; align-items: center; justify-content: center;">
  <div style="background: white; border-radius: 16px; box-shadow: 0 25px 50px rgba(0,0,0,0.25); padding: 32px; width: 384px;">
    <h2 style="font-size: 1.5rem; font-weight: 700; text-align: center;">Welcome Back</h2>
    <input style="width: 100%; margin-top: 16px; padding: 12px; border: 1px solid #d1d5db; border-radius: 8px;" placeholder="Email" />
    <input style="width: 100%; margin-top: 12px; padding: 12px; border: 1px solid #d1d5db; border-radius: 8px;" placeholder="Password" type="password" />
    <button style="width: 100%; margin-top: 16px; background: #6366f1; color: white; padding: 12px; border-radius: 8px; font-weight: 700; border: none;">
      Sign In
    </button>
  </div>
</div>
```

**✅ PRO:**
```html
<div style="min-height: 100vh; display: flex; align-items: center; justify-content: center; background: var(--color-background); padding: var(--space-4);">
  <div class="card" style="width: 100%; max-width: 400px; padding: var(--space-8);">
    <div style="text-align: center;">
      <h2 style="font-size: 1.25rem; font-weight: 600; color: var(--color-text);">Welcome back</h2>
      <p style="font-size: 0.875rem; color: var(--color-muted); margin-top: var(--space-1);">Sign in to your account</p>
    </div>
    <form style="margin-top: var(--space-6); space-y: var(--space-4);">
      <div class="field">
        <label for="email" class="field__label">Email <span class="field__required">*</span></label>
        <input id="email" type="email" class="field__input" placeholder="you@example.com" required />
      </div>
      <div class="field">
        <div style="display: flex; align-items: center; justify-content: space-between;">
          <label for="password" class="field__label">Password</label>
          <a href="/forgot" style="font-size: 0.75rem; color: var(--color-primary);">Forgot?</a>
        </div>
        <input id="password" type="password" class="field__input" required />
      </div>
      <button type="submit" class="btn btn--primary" style="width: 100%;">Sign in</button>
    </form>
    <p style="margin-top: var(--space-4); text-align: center; font-size: 0.875rem; color: var(--color-muted);">
      Don't have an account? <a href="/signup" style="color: var(--color-primary);">Sign up</a>
    </p>
  </div>
</div>
```

**What changed (universal):**
- Gradient background → `var(--color-background)` (respects theme)
- Placeholder-only → visible `<label>` above each input
- No forgot password → recovery path
- No signup link → clear next step
- Semantic form with proper `type` attributes

---

## 🚨 Anti-Slop Quick Check

Before delivering, scan for these. If ANY match → fix:

| Pattern | Symptom |
|---|---|
| Dashboard gradient | Gratuitous gradient on cards/headers — use solid bg or subtle border instead |
| Padding explosion | Inconsistent padding (12px, 16px, 24px mixed) — pick from spacing scale |
| Center everything | All text centered, no visual hierarchy — left-align body, center only hero headlines |
| Icon soup | Emoji/icon on every element — icons only for actions, not decoration |
| Card-itis | Everything wrapped in cards — use sections/dividers, not cards for text content |
| Font salad | 3+ font families — max 2 (display + body) |
| Hover-only | Functionality only on hover — add visible state + touch-friendly hit area |
| No focus | Missing keyboard focus indicators — add `:focus-visible` ring |
| Placeholder labels | Using placeholder as only label — add `<label>` above input |
| Dark gradient cards | Black/dark cards with gradient — use white/light cards with subtle border |
| Hex color soup | Hardcoded hex values (#3b82f6, #ef4444) — use CSS custom properties or Tailwind tokens |
| No semantic colors | All changes green — use emerald for positive, red for negative |
| Centered form | Login/signup centered with no context — add header, description, recovery links |

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

**If using shadcn/ui:** Use its token system (text-foreground, text-muted-foreground, bg-background, etc.). Do NOT invent custom hex values.
**If using Tailwind:** Use its default palette. Do NOT use arbitrary values like `[#3b82f6]` when `bg-blue-600` exists.

---

## ✅ Positive Design Patterns (Agnostic — Any Stack)

These are the patterns that separate professional UI from template UI.
The PRINCIPLES are universal. Syntax changes per stack.

### Card Pattern
```html
<!-- ✅ Professional: subtle border, clear hierarchy, semantic tokens -->
<div class="card">
  <h3 class="card__title">Title</h3>
  <p class="card__description">Description</p>
  <div class="card__content">Content</div>
  <div class="card__footer">Action</div>
</div>

<!-- ❌ Template: dark gradient, no hierarchy, generic -->
<div style="background: linear-gradient(135deg, #1f2937, #111827); border-radius: 12px; padding: 24px; color: white;">
  <p style="font-size: 1.5rem; font-weight: 700;">Title</p>
</div>
```

### Stats/Dashboard Pattern
```html
<!-- ✅ Professional: white card, muted label, bold value, semantic change color -->
<div class="card">
  <div style="display: flex; align-items: center; justify-content: space-between; padding-bottom: var(--space-2);">
    <p style="font-size: 0.875rem; font-weight: 500; color: var(--color-muted);">Revenue</p>
    <svg style="color: var(--color-muted);">$</svg>
  </div>
  <p style="font-size: 1.5rem; font-weight: 700;">$45,231</p>
  <p style="font-size: 0.75rem; color: var(--color-muted); margin-top: var(--space-1);">
    <span style="color: var(--color-success);">+20.1%</span> from last month
  </p>
</div>
```

### Form Pattern
```html
<!-- ✅ Professional: visible labels, helper text, error states, semantic layout -->
<div class="field">
  <label for="email" class="field__label">Email <span class="field__required">*</span></label>
  <input id="email" type="email" class="field__input" required aria-describedby="email-help" />
  <p id="email-help" class="field__helper">We'll never share your email.</p>
</div>
```

### Navigation Pattern
```html
<!-- ✅ Professional: max 7 items, active state via ONE method, consistent -->
<nav style="display: flex; align-items: center; gap: var(--space-6);">
  <a href="/" style="font-size: 1.125rem; font-weight: 600; color: var(--color-text);">Logo</a>
  <div style="display: flex; gap: var(--space-1);">
    <a href="/dashboard" class="nav-link nav-link--active">Dashboard</a>
    <a href="/settings" class="nav-link">Settings</a>
    <a href="/billing" class="nav-link">Billing</a>
  </div>
</nav>
```
```css
.nav-link {
  padding: var(--space-2) var(--space-3);
  font-size: 0.875rem;
  border-radius: var(--radius-md);
  color: var(--color-muted);
  text-decoration: none;
  transition: all 150ms ease-out;
}
.nav-link:hover { color: var(--color-text); background: var(--color-muted-bg); }
.nav-link--active { color: var(--color-text); background: var(--color-muted-bg); font-weight: 500; }
```

### Empty State Pattern
```html
<!-- ✅ Professional: icon + message + CTA, not just text -->
<div style="display: flex; flex-direction: column; align-items: center; justify-content: center; padding: var(--space-12) 0; text-align: center;">
  <svg style="width: 48px; height: 48px; color: var(--color-muted); margin-bottom: var(--space-4);">📭</svg>
  <h3 style="font-size: 1.125rem; font-weight: 600; color: var(--color-text);">No messages yet</h3>
  <p style="font-size: 0.875rem; color: var(--color-muted); margin-top: var(--space-1); max-width: 400px;">
    Start a conversation to see messages here.
  </p>
  <button class="btn btn--primary" style="margin-top: var(--space-4);">Start chat</button>
</div>
```

### Color Usage Rules
```html
<!-- ✅ Professional: semantic color tokens -->
<span style="color: var(--color-text);">Primary text</span>
<span style="color: var(--color-muted);">Secondary text</span>
<span style="color: var(--color-success);">+12.5% (positive)</span>
<span style="color: var(--color-destructive);">-3.2% (negative)</span>
<span class="badge badge--secondary">Status</span>

<!-- ❌ Template: hardcoded hex values -->
<span style="color: #3b82f6;">Text</span>
<span style="color: #22c55e;">+12.5%</span>
```

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
