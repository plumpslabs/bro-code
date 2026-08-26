# Stack-Specific Reference

Load for stack-specific implementation rules. Detect stack from project files first.

---

## Detection

| Stack | Signal |
|---|---|
| **Plain HTML/CSS** | `.html` + `.css` files, no framework config |
| **Tailwind CSS** | `tailwindcss` in package.json or `tailwind.config.*` |
| **React** | `react` in package.json |
| **Next.js** | `next` in package.json |
| **Vue** | `vue` in package.json |
| **Nuxt** | `nuxt` in package.json |
| **Svelte** | `svelte` in package.json |
| **Laravel/Blade** | `composer.json` with `laravel`, `.blade.php` files |
| **Ruby on Rails** | `Gemfile` with `rails`, `.erb`/`.haml`/`.slim` files |
| **Django** | `requirements.txt` with `django`, `.html` templates |
| **Flask** | `requirements.txt` with `flask`, Jinja2 templates |
| **Flutter** | `pubspec.yaml` with `flutter` |
| **SwiftUI** | `*.xcodeproj` or `Package.swift` with SwiftUI |
| **PHP (plain)** | `.php` files without Laravel |
| **WordPress** | `wp-config.php`, `functions.php`, theme files |

---

## Universal Rules (ALL Stacks)

These apply regardless of framework. Every UI task MUST follow these.

| # | Rule | Why |
|---|---|---|
| U1 | Define CSS custom properties (design tokens) before writing component styles | Consistency |
| U2 | Use semantic color names (`--color-primary`, `--color-muted`), not hex values | Maintainability |
| U3 | Spacing from a scale (4/8/12/16/24/32/48/64px), not random values | Rhythm |
| U4 | Typography hierarchy: display → heading → body → caption (max 2 font families) | Readability |
| U5 | Border-radius from a scale (`--radius-sm/md/lg/xl`), not random px | Consistency |
| U6 | All interactive elements have visible focus indicator (`:focus-visible`) | Accessibility |
| U7 | Responsive: mobile-first, test at 375px / 768px / 1280px | Compatibility |
| U8 | No inline styles except truly dynamic values (JS-computed) | Maintainability |

---

## Plain HTML/CSS

| Rule | Severity |
|---|---|
| Use CSS custom properties for all design tokens | MUST |
| BEM naming convention (`block__element--modifier`) | SHOULD |
| Mobile-first media queries: `@media (min-width: 768px)` | MUST |
| `rem` for font sizes, `px` for borders only | SHOULD |
| `:focus-visible` for keyboard navigation | MUST |
| `prefers-reduced-motion` for animations | SHOULD |
| Semantic HTML: `<button>`, `<nav>`, `<main>`, `<section>` — not `<div>` everything | MUST |

**Token system (paste into `:root`):**
```css
:root {
  /* Colors */
  --color-primary: oklch(0.45 0.18 260);
  --color-primary-hover: oklch(0.40 0.18 260);
  --color-surface: #ffffff;
  --color-background: #f8fafc;
  --color-text: #0f172a;
  --color-muted: #64748b;
  --color-border: #e2e8f0;
  --color-destructive: oklch(0.55 0.2 25);

  /* Typography */
  --font-display: 'Inter', system-ui, sans-serif;
  --font-body: 'Inter', system-ui, sans-serif;
  --font-mono: 'JetBrains Mono', monospace;

  /* Spacing (4px base) */
  --space-1: 4px;
  --space-2: 8px;
  --space-3: 12px;
  --space-4: 16px;
  --space-5: 20px;
  --space-6: 24px;
  --space-8: 32px;
  --space-10: 40px;
  --space-12: 48px;
  --space-16: 64px;

  /* Border Radius */
  --radius-sm: 4px;
  --radius-md: 8px;
  --radius-lg: 12px;
  --radius-xl: 16px;
  --radius-full: 9999px;

  /* Shadows */
  --shadow-sm: 0 1px 2px rgba(0,0,0,0.05);
  --shadow-md: 0 4px 6px -1px rgba(0,0,0,0.1);
  --shadow-lg: 0 10px 15px -3px rgba(0,0,0,0.1);
}
```

**Button pattern:**
```css
.btn {
  display: inline-flex;
  align-items: center;
  justify-content: center;
  padding: var(--space-2) var(--space-4);
  font-size: 0.875rem;
  font-weight: 500;
  border-radius: var(--radius-md);
  border: 1px solid transparent;
  cursor: pointer;
  transition: all 150ms ease-out;
}
.btn:focus-visible {
  outline: 2px solid var(--color-primary);
  outline-offset: 2px;
}
.btn:disabled {
  opacity: 0.5;
  cursor: not-allowed;
}
.btn--primary {
  background: var(--color-primary);
  color: white;
}
.btn--primary:hover {
  background: var(--color-primary-hover);
}
.btn--outline {
  background: transparent;
  border-color: var(--color-border);
  color: var(--color-text);
}
```

---

## Tailwind CSS (Standalone or with any framework)

| Rule | Severity |
|---|---|
| Use utility classes, not custom CSS unless necessary | MUST |
| Consistent spacing: `p-4`=16px, `gap-6`=24px, `m-2`=8px | MUST |
| Responsive: `sm:`, `md:`, `lg:`, `xl:` prefixes | MUST |
| Dark mode: `dark:` prefix | SHOULD |
| No arbitrary values (`[12px]`) unless from design token | SHOULD |
| `@apply` only for repeated patterns (3+ occurrences) | SHOULD |
| Use `cn()` or `clsx()` for conditional classes | SHOULD |

**Token system (tailwind.config.js):**
```js
// tailwind.config.js
module.exports = {
  theme: {
    extend: {
      colors: {
        primary: {
          DEFAULT: 'oklch(0.45 0.18 260)',
          hover: 'oklch(0.40 0.18 260)',
        },
        surface: '#ffffff',
        muted: '#64748b',
      },
      borderRadius: {
        sm: '4px',
        md: '8px',
        lg: '12px',
        xl: '16px',
      },
    },
  },
}
```

**Button pattern:**
```html
<!-- Primary -->
<button class="px-4 py-2 text-sm font-medium rounded-md
               bg-primary text-white
               hover:bg-primary/90
               focus-visible:outline-2 focus-visible:outline-offset-2 focus-visible:outline-primary
               disabled:opacity-50 disabled:cursor-not-allowed
               transition-colors">
  Save changes
</button>

<!-- Outline -->
<button class="px-4 py-2 text-sm font-medium rounded-md
               border border-border bg-transparent
               hover:bg-muted/10
               focus-visible:outline-2 focus-visible:outline-offset-2 focus-visible:outline-primary">
  Cancel
</button>
```

---

## Laravel / Blade

| Rule | Severity |
|---|---|
| Use `@extends` / `@section` for layout consistency | MUST |
| Blade components: `<x-button>`, `<x-input>`, `<x-card>` | SHOULD |
| Tailwind for styling (Laravel default since 2024) | SHOULD |
| `wire:` / Alpine.js for interactivity, not jQuery | SHOULD |
| `@error` directive for form validation display | MUST |
| Route names: `{{ route('billing.index') }}` not hardcoded URLs | MUST |

**Component pattern:**
```blade
{{-- resources/views/components/card.blade.php --}}
@props(['title' => null, 'description' => null])

<div class="rounded-lg border border-border bg-surface p-6 shadow-sm">
    @if ($title)
        <h3 class="text-lg font-semibold text-foreground">{{ $title }}</h3>
    @endif
    @if ($description)
        <p class="mt-1 text-sm text-muted">{{ $description }}</p>
    @endif
    <div class="mt-4">
        {{ $slot }}
    </div>
</div>

{{-- Usage --}}
<x-card title="Revenue" description="Monthly recurring revenue">
    <p class="text-2xl font-bold">$45,231</p>
</x-card>
```

**Form pattern:**
```blade
<div class="space-y-2">
    <label for="email" class="text-sm font-medium text-foreground">
        Email <span class="text-destructive">*</span>
    </label>
    <input
        id="email"
        type="email"
        name="email"
        value="{{ old('email') }}"
        class="w-full rounded-md border border-border px-3 py-2 text-sm
               focus:outline-2 focus:outline-offset-2 focus:outline-primary
               @error('email') border-destructive @enderror"
        required
    />
    @error('email')
        <p class="text-sm text-destructive">{{ $message }}</p>
    @enderror
</div>
```

---

## Ruby on Rails

| Rule | Severity |
|---|---|
| Use view components (`ViewComponent` gem) for reusable UI | SHOULD |
| Tailwind or Hotwire for styling/interactivity | SHOULD |
| `form_with` / `form_for` for forms (not raw HTML) | MUST |
| Partials for repeated UI chunks | SHOULD |
| `content_for` for layout sections | SHOULD |
| Stimulus controllers for JS behavior | SHOULD |

**Component pattern:**
```erb
<%# app/views/shared/_card.html.erb %>
<%# locals: (title: nil, description: nil, &block) %>

<div class="rounded-lg border border-gray-200 bg-white p-6 shadow-sm">
  <% if title %>
    <h3 class="text-lg font-semibold text-gray-900"><%= title %></h3>
  <% end %>
  <% if description %>
    <p class="mt-1 text-sm text-gray-500"><%= description %></p>
  <% end %>
  <div class="mt-4">
    <%= yield %>
  </div>
</div>

<%# Usage %>
<%= render "shared/card", title: "Revenue", description: "Monthly recurring revenue" do %>
  <p class="text-2xl font-bold">$45,231</p>
<% end %>
```

**Form pattern:**
```erb
<%= form_with(model: @user, class: "space-y-4") do |form| %>
  <div class="space-y-1">
    <%= form.label :email, class: "text-sm font-medium text-gray-900" %>
    <%= form.email_field :email,
        class: "w-full rounded-md border border-gray-300 px-3 py-2 text-sm
                focus:outline-2 focus:outline-offset-2 focus:outline-blue-600
                #{'border-red-500' if @user.errors[:email].any?}",
        required: true %>
    <% @user.errors[:email].each do |error| %>
      <p class="text-sm text-red-600"><%= error %></p>
    <% end %>
  </div>
<% end %>
```

---

## Django

| Rule | Severity |
|---|---|
| Use Django widgets or `django-crispy-forms` for forms | SHOULD |
| `{% extends %}` / `{% block %}` for layout | MUST |
| Tailwind or class-based styling (not inline) | SHOULD |
| `{% csrf_token %}` in all forms | MUST |
| `|safe` only for trusted HTML | MUST |
| Static files: `{% static 'css/app.css' %}` | MUST |

**Component pattern:**
```django
{# templates/components/card.html #}
{% load ui_tags %}

<div class="rounded-lg border border-gray-200 bg-white p-6 shadow-sm">
    {% if title %}
        <h3 class="text-lg font-semibold text-gray-900">{{ title }}</h3>
    {% endif %}
    {% if description %}
        <p class="mt-1 text-sm text-gray-500">{{ description }}</p>
    {% endif %}
    <div class="mt-4">
        {% block content %}{% endblock %}
    </div>
</div>
```

---

## React / Next.js

| Rule | Severity |
|---|---|
| Use CSS Modules or Tailwind, not inline styles | SHOULD |
| `className` for styling, `style` only for dynamic values | SHOULD |
| Next.js: use `next/image` for optimized images | SHOULD |
| Avoid `useEffect` for animations — use CSS or Framer Motion | SHOULD |
| Don't put large objects in JSX props (causes re-render) | SHOULD |
| shadcn/ui primitives for consistent component library | SHOULD |

---

## Vue / Nuxt

| Rule | Severity |
|---|---|
| Use `<style scoped>` or Tailwind | SHOULD |
| `v-bind` in `<style>` for dynamic styles | SHOULD |
| Nuxt: use `<NuxtImg>` for optimized images | SHOULD |
| Composition API, not Options API (for new code) | SHOULD |

---

## Svelte / SvelteKit

| Rule | Severity |
|---|---|
| Use scoped `<style>` (default in Svelte) | SHOULD |
| `{#if}`, `{#each}` for conditional/list rendering | SHOULD |
| Svelte transitions: `transition:fade`, `in:fly` | SHOULD |
| Use `$$props` sparingly — prefer explicit props | SHOULD |

---

## Flutter / Dart

| Rule | Severity |
|---|---|
| Use `Theme.of(context)` for design tokens | MUST |
| `SizedBox` for spacing, not `Container` | SHOULD |
| `Text` with `TextStyle` from theme, not hardcoded colors | MUST |
| `ListView.builder` for long lists (not `ListView(children:)`) | MUST |
| Material 3 (`useMaterial3: true`) for modern design | SHOULD |

---

## SwiftUI

| Rule | Severity |
|---|---|
| Use `@Environment` for theme values | SHOULD |
| `padding(.horizontal, 16)` not hardcoded pixels | SHOULD |
| `ScrollView` + `VStack` for scrollable content | SHOULD |
| `LazyVStack` for long lists | SHOULD |

---

## WordPress

| Rule | Severity |
|---|---|
| Use `get_template_part()` for reusable templates | MUST |
| Tailwind or theme.json for styling (FSE themes) | SHOULD |
| `wp_enqueue_style()` for stylesheets (not inline `<link>`) | MUST |
| Block themes: use `theme.json` for design tokens | SHOULD |
| Classic themes: use `functions.php` for enqueuing | MUST |
| `esc_html()`, `esc_attr()`, `esc_url()` for output escaping | MUST |

---

## Code Examples (Agnostic)

### Card — All Stacks

**HTML:**
```html
<div class="card">
  <h3 class="card__title">Revenue</h3>
  <p class="card__description">Monthly recurring revenue</p>
  <div class="card__content">
    <p class="text-2xl font-bold">$45,231</p>
  </div>
</div>
```

**CSS:**
```css
.card {
  padding: var(--space-6);
  border-radius: var(--radius-lg);
  border: 1px solid var(--color-border);
  background: var(--color-surface);
  box-shadow: var(--shadow-sm);
}
.card__title {
  font-size: 1.125rem;
  font-weight: 600;
  color: var(--color-text);
}
.card__description {
  margin-top: var(--space-1);
  font-size: 0.875rem;
  color: var(--color-muted);
}
```

**Tailwind:**
```html
<div class="rounded-lg border border-border bg-surface p-6 shadow-sm">
  <h3 class="text-lg font-semibold text-foreground">Revenue</h3>
  <p class="mt-1 text-sm text-muted">Monthly recurring revenue</p>
  <div class="mt-4">
    <p class="text-2xl font-bold">$45,231</p>
  </div>
</div>
```

### Button — All Stacks

**HTML + CSS:**
```html
<button class="btn btn--primary">Save changes</button>
<button class="btn btn--outline">Cancel</button>
<button class="btn btn--ghost">Learn more</button>
<button class="btn btn--danger">Delete</button>
```

**Tailwind:**
```html
<button class="px-4 py-2 text-sm font-medium rounded-md
               bg-primary text-white hover:bg-primary/90
               focus-visible:outline-2 focus-visible:outline-offset-2
               focus-visible:outline-primary
               disabled:opacity-50 disabled:cursor-not-allowed
               transition-colors">
  Save changes
</button>
```

### Form — All Stacks

**HTML + CSS:**
```html
<div class="field">
  <label for="email" class="field__label">
    Email <span class="field__required">*</span>
  </label>
  <input
    id="email"
    type="email"
    class="field__input"
    aria-describedby="email-help"
    required
  />
  <p id="email-help" class="field__helper">We'll never share your email.</p>
  <p class="field__error" role="alert">Please enter a valid email.</p>
</div>
```

**CSS:**
```css
.field { margin-bottom: var(--space-4); }
.field__label {
  display: block;
  font-size: 0.875rem;
  font-weight: 500;
  color: var(--color-text);
  margin-bottom: var(--space-1);
}
.field__required { color: var(--color-destructive); }
.field__input {
  width: 100%;
  padding: var(--space-2) var(--space-3);
  border: 1px solid var(--color-border);
  border-radius: var(--radius-md);
  font-size: 0.875rem;
}
.field__input:focus {
  outline: 2px solid var(--color-primary);
  outline-offset: 2px;
}
.field__helper {
  font-size: 0.75rem;
  color: var(--color-muted);
  margin-top: var(--space-1);
}
.field__error {
  font-size: 0.75rem;
  color: var(--color-destructive);
  margin-top: var(--space-1);
}
```

**Tailwind:**
```html
<div class="space-y-2">
  <label for="email" class="text-sm font-medium text-foreground">
    Email <span class="text-destructive">*</span>
  </label>
  <input
    id="email"
    type="email"
    class="w-full rounded-md border border-border px-3 py-2 text-sm
           focus:outline-2 focus:outline-offset-2 focus:outline-primary"
    aria-describedby="email-help"
    required
  />
  <p id="email-help" class="text-xs text-muted">We'll never share your email.</p>
</div>
```
