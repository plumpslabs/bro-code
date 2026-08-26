# Animation Reference

Load for complex animations, scroll effects, GSAP work.

---

## Timing

| Type | Duration | Use |
|---|---|---|
| Micro-interaction | 150-200ms | Button hover, toggle, focus |
| Page transition | 300-400ms | Route changes, modals |
| Complex animation | 500-700ms | Page-load sequences |

## Rules

| # | Rule | Severity |
|---|---|---|
| AN1 | Enter faster than exit (200ms vs 150ms) | SHOULD |
| AN2 | Stagger list items by 50-100ms | NICE |
| AN3 | Easing: `cubic-bezier(0.4, 0, 0.2, 1)` for most UI | SHOULD |
| AN4 | Never animate width/height (use transform) | MUST |
| AN5 | Always respect `prefers-reduced-motion` | MUST |
| AN6 | Animation must convey meaning, not decorate | SHOULD |

## Anti-Patterns

| Pattern | Problem | Fix |
|---|---|---|
| Animate width/height | Layout thrashing, jank | Use `transform: scale()` |
| One duration for everything | Feels robotic | Vary by type (micro/transition/complex) |
| Animate display/visibility | Elements pop in | Use `opacity` + `transform` |
| No reduced motion | Accessibility violation | Add `@media (prefers-reduced-motion)` |
| Everything animates | Visual noise | Pick 1-2 hero animations per page |

## Code Examples

### Button Hover (Micro)
```css
.button {
  transition: transform 150ms cubic-bezier(0.4, 0, 0.2, 1),
              box-shadow 150ms cubic-bezier(0.4, 0, 0.2, 1);
}
.button:hover {
  transform: translateY(-1px);
  box-shadow: 0 4px 12px rgba(0,0,0,0.15);
}
```

### Modal Enter/Exit
```css
.modal {
  opacity: 0;
  transform: scale(0.95);
  transition: opacity 200ms ease-out, transform 200ms ease-out;
}
.modal.open {
  opacity: 1;
  transform: scale(1);
}
/* Exit is faster than enter */
.modal.closing {
  transition-duration: 150ms;
}
```

### List Stagger
```css
@keyframes fadeInUp {
  from { opacity: 0; transform: translateY(8px); }
  to { opacity: 1; transform: translateY(0); }
}
.list-item {
  animation: fadeInUp 300ms ease-out both;
}
.list-item:nth-child(1) { animation-delay: 0ms; }
.list-item:nth-child(2) { animation-delay: 50ms; }
.list-item:nth-child(3) { animation-delay: 100ms; }
```

### Reduced Motion Fallback
```css
@media (prefers-reduced-motion: reduce) {
  .button { transition: none; }
  .modal { transition: opacity 150ms ease-out; transform: none; }
  .list-item { animation: none; opacity: 1; }
}
```
