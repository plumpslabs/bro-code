# Component Patterns Reference (Agnostic)

Load when building buttons, forms, cards, navigation, modals.
**These patterns work in ANY stack** — HTML/CSS, Tailwind, React, Vue, Blade, Rails, Django, Flutter, SwiftUI.

---

## Buttons

| Type | Style | Use |
|---|---|---|
| Primary | Filled, high contrast | ONE per section — main action |
| Secondary | Outlined or ghost | Supporting actions |
| Tertiary | Text-only, low-emphasis | Links, secondary info |
| Danger | Red bg or red text | Destructive actions |

**Rules:**
- Consistent padding: `--space-3 --space-6` (12px 24px)
- Border-radius: match your system (`--radius-md`)
- Font: same as body, weight 500-600
- ONE primary per section (not two competing CTAs)
- Focus visible on ALL buttons (keyboard nav)

**HTML + CSS:**
```html
<button class="btn btn--primary">Primary action</button>
<button class="btn btn--outline">Secondary</button>
<button class="btn btn--ghost">Tertiary</button>
<button class="btn btn--danger">Delete</button>
```
```css
.btn {
  display: inline-flex; align-items: center; justify-content: center;
  padding: var(--space-3) var(--space-6);
  font-size: 0.875rem; font-weight: 500;
  border-radius: var(--radius-md);
  border: 1px solid transparent;
  cursor: pointer;
  transition: all 150ms ease-out;
}
.btn:focus-visible {
  outline: 2px solid var(--color-primary);
  outline-offset: 2px;
}
.btn:disabled { opacity: 0.5; cursor: not-allowed; }
.btn--primary { background: var(--color-primary); color: white; }
.btn--primary:hover { background: var(--color-primary-hover); }
.btn--outline { background: transparent; border-color: var(--color-border); color: var(--color-text); }
.btn--ghost { background: transparent; color: var(--color-muted); }
.btn--ghost:hover { background: var(--color-muted-bg); }
.btn--danger { background: var(--color-destructive); color: white; }
```

**Tailwind:**
```html
<button class="px-4 py-2 text-sm font-medium rounded-md bg-primary text-white hover:bg-primary/90 focus-visible:outline-2 focus-visible:outline-offset-2 focus-visible:outline-primary transition-colors">Primary</button>
<button class="px-4 py-2 text-sm font-medium rounded-md border border-border bg-transparent hover:bg-muted/10 focus-visible:outline-2 focus-visible:outline-offset-2 focus-visible:outline-primary transition-colors">Secondary</button>
```

**shadcn/ui (React):**
```tsx
<Button>Primary</Button>
<Button variant="secondary">Secondary</Button>
<Button variant="outline">Outline</Button>
<Button variant="ghost">Ghost</Button>
<Button variant="destructive">Delete</Button>
```

**Blade (Laravel):**
```blade
<x-button variant="primary">Primary</x-button>
<x-button variant="outline">Secondary</x-button>
```

---

## Forms

| Element | Position | Rules |
|---|---|---|
| Label | ABOVE input | Always visible, never placeholder-only |
| Input | Below label | Min height 44px for touch |
| Helper | Below input | Gray, smaller font |
| Error | Below helper | Red, with icon, on blur validation |

**Rules:**
- Validation: inline on blur (not every keystroke)
- Required fields: asterisk (*) with "required" in label
- Disabled state: visual dim + `aria-disabled`
- `autocomplete` attributes for common fields (email, name, password)

**HTML + CSS:**
```html
<div class="field">
  <label for="email" class="field__label">Email <span class="field__required">*</span></label>
  <input id="email" type="email" class="field__input" autocomplete="email" required />
  <p class="field__helper">We'll never share your email.</p>
</div>
```

**Tailwind:**
```html
<div class="space-y-2">
  <label for="email" class="text-sm font-medium text-foreground">Email <span class="text-destructive">*</span></label>
  <input id="email" type="email" class="w-full rounded-md border border-border px-3 py-2 text-sm focus:outline-2 focus:outline-offset-2 focus:outline-primary" autocomplete="email" required />
  <p class="text-xs text-muted">We'll never share your email.</p>
</div>
```

---

## Cards

| Rule | Severity |
|---|---|
| Use consistent card pattern across all cards | MUST |
| White/light background with subtle border | MUST |
| No dark gradient backgrounds | MUST |
| Consistent padding across all cards | MUST |
| ONE hover effect per card type (lift OR border OR shadow) | SHOULD |
| Consistent border-radius | MUST |
| Lock image aspect ratios | SHOULD |

**HTML + CSS:**
```html
<div class="card">
  <h3 class="card__title">Title</h3>
  <p class="card__description">Description</p>
  <div class="card__content">Content</div>
  <div class="card__footer">Action</div>
</div>
```
```css
.card {
  padding: var(--space-6);
  border-radius: var(--radius-lg);
  border: 1px solid var(--color-border);
  background: var(--color-surface);
  box-shadow: var(--shadow-sm);
  transition: box-shadow 200ms ease-out;
}
.card:hover { box-shadow: var(--shadow-md); }
.card__title { font-size: 1.125rem; font-weight: 600; color: var(--color-text); }
.card__description { margin-top: var(--space-1); font-size: 0.875rem; color: var(--color-muted); }
.card__content { margin-top: var(--space-4); }
.card__footer { margin-top: var(--space-6); }
```

**Tailwind:**
```html
<div class="rounded-lg border border-border bg-surface p-6 shadow-sm hover:shadow-md transition-shadow">
  <h3 class="text-lg font-semibold text-foreground">Title</h3>
  <p class="mt-1 text-sm text-muted">Description</p>
  <div class="mt-4">Content</div>
  <div class="mt-6">Action</div>
</div>
```
| ONE hover effect per card type (lift OR border OR shadow) | SHOULD |
| Consistent border-radius | MUST |
| Lock image aspect ratios | SHOULD |
| White/light background with subtle border | MUST |
| No dark gradient backgrounds | MUST |

**shadcn/ui pattern:**
```tsx
import { Card, CardHeader, CardTitle, CardDescription, CardContent, CardFooter } from '@/components/ui/card'

<Card>
  <CardHeader>
    <CardTitle>Title</CardTitle>
    <CardDescription>Description</CardDescription>
  </CardHeader>
  <CardContent>Content here</CardContent>
  <CardFooter>
    <Button>Action</Button>
  </CardFooter>
</Card>
```

## Navigation

| Pattern | When |
|---|---|
| Top nav (logo left, links right) | Most websites |
| Sidebar | Dashboards, admin panels |
| Bottom nav (≤5 items) | Mobile apps |
| Hamburger | Mobile fallback |

**Rules:**
- Max 7 top-level items (Miller's Law)
- Active state: bold OR underline OR background — pick ONE
- Consistent active indicator across all nav items

## Code Examples

### Form with Error State
```html
<div class="field">
  <label for="email">Email <span aria-hidden="true">*</span></label>
  <input id="email" type="email" required aria-describedby="email-help email-error" />
  <p id="email-help" class="helper">We'll never share your email.</p>
  <p id="email-error" class="error" role="alert">
    <svg aria-hidden="true">...</svg> Please enter a valid email address.
  </p>
</div>
```

### Button States
```css
.button {
  /* Base */
  padding: var(--space-3) var(--space-6);
  border-radius: var(--radius-md);
  font-weight: 500;
  transition: all 150ms ease-out;
}
.button:hover { opacity: 0.9; }
.button:focus-visible { outline: 2px solid var(--color-primary); outline-offset: 2px; }
.button:active { transform: scale(0.98); }
.button:disabled { opacity: 0.5; cursor: not-allowed; }
```

### Card with Consistent Spacing
```css
.card {
  padding: var(--space-6);
  border-radius: var(--radius-lg);
  border: 1px solid var(--color-border);
  transition: box-shadow 200ms ease-out;
}
.card:hover {
  box-shadow: 0 4px 16px rgba(0,0,0,0.08);
}
.card img {
  aspect-ratio: 16/9;
  object-fit: cover;
  border-radius: var(--radius-md) var(--radius-md) 0 0;
}
```
