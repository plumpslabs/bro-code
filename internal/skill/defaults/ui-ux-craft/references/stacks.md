# Stack-Specific Reference

Load for stack-specific implementation rules. Detect stack from project files first.

---

## Detection

| Stack | Signal |
|---|---|
| React | `react` in package.json |
| Next.js | `next` in package.json |
| Vue | `vue` in package.json |
| Nuxt | `nuxt` in package.json |
| Svelte | `svelte` in package.json |
| Flutter | `pubspec.yaml` with `flutter` |
| SwiftUI | `*.xcodeproj` or `Package.swift` with SwiftUI |
| Tailwind | `tailwindcss` in package.json or config |

---

## React / Next.js

| Rule | Severity |
|---|---|
| Use CSS Modules or Tailwind, not inline styles | SHOULD |
| `className` for styling, `style` only for dynamic values | SHOULD |
| Next.js: use `next/image` for optimized images | SHOULD |
| Avoid `useEffect` for animations — use CSS or Framer Motion | SHOULD |
| Don't put large objects in JSX props (causes re-render) | SHOULD |

## Vue / Nuxt

| Rule | Severity |
|---|---|
| Use `<style scoped>` or Tailwind | SHOULD |
| `v-bind` in `<style>` for dynamic styles | SHOULD |
| Nuxt: use `<NuxtImg>` for optimized images | SHOULD |

## Tailwind CSS

| Rule | Severity |
|---|---|
| Use utility classes, not custom CSS unless necessary | SHOULD |
| Consistent spacing: `p-4`=16px, `gap-6`=24px | SHOULD |
| Responsive: `md:`, `lg:`, `xl:` prefixes | SHOULD |
| Dark mode: `dark:` prefix | SHOULD |
| No arbitrary values unless justified | NICE |

## Flutter / Dart

| Rule | Severity |
|---|---|
| Use `Theme.of(context)` for design tokens | SHOULD |
| `SizedBox` for spacing, not `Container` | SHOULD |
| `Text` with `TextStyle` from theme, not hardcoded colors | SHOULD |
| `ListView.builder` for long lists (not `ListView(children:)`) | SHOULD |

## SwiftUI

| Rule | Severity |
|---|---|
| Use `@Environment` for theme values | SHOULD |
| `padding(.horizontal, 16)` not hardcoded pixels | SHOULD |
| `ScrollView` + `VStack` for scrollable content | SHOULD |
| `LazyVStack` for long lists | SHOULD |

## Code Example (React + Tailwind)
```tsx
// Good: consistent tokens, semantic, accessible
<button
  className="px-4 py-2 rounded-md bg-primary text-white 
             hover:bg-primary/90 focus-visible:outline-2 
             focus-visible:outline-offset-2 
             focus-visible:outline-primary"
  aria-label="Save changes"
>
  Save changes
</button>

// Bad: inline styles, no focus, no aria
<button style={{padding: '12px 24px', background: '#3b82f6'}}>
  Submit
</button>
```
