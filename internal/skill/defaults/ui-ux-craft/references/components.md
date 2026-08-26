# Component Patterns Reference

Load when building buttons, forms, cards, navigation, modals.

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

## Cards

| Rule | Severity |
|---|---|
| Consistent padding across all cards | MUST |
| ONE hover effect per card type (lift OR border OR shadow) | SHOULD |
| Consistent border-radius | MUST |
| Lock image aspect ratios | SHOULD |

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
