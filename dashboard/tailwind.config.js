/** @type {import('tailwindcss').Config} */
export default {
  content: [
    './index.html',
    './src/**/*.{ts,tsx}',
  ],
  theme: {
    extend: {
      colors: {
        pipeline: {
          intake: '#6366f1',
          prd: '#8b5cf6',
          research: '#3b82f6',
          planning: '#06b6d4',
          implementation: '#10b981',
          review: '#f59e0b',
          human_review: '#f97316',
          qa: '#84cc16',
          passed: '#22c55e',
          failed: '#ef4444',
          blocked: '#dc2626',
          rejected: '#9f1239',
          released: '#14b8a6',
        },
        surface: {
          50: 'rgb(var(--color-surface-50) / <alpha-value>)',
          100: 'rgb(var(--color-surface-100) / <alpha-value>)',
          200: 'rgb(var(--color-surface-200) / <alpha-value>)',
          700: 'rgb(var(--color-surface-700) / <alpha-value>)',
          800: 'rgb(var(--color-surface-800) / <alpha-value>)',
          850: 'rgb(var(--color-surface-850) / <alpha-value>)',
          900: 'rgb(var(--color-surface-900) / <alpha-value>)',
          950: 'rgb(var(--color-surface-950) / <alpha-value>)',
        },
        accent: {
          DEFAULT: 'rgb(var(--color-accent) / <alpha-value>)',
          dim: 'rgb(var(--color-accent-dim) / <alpha-value>)',
          hover: 'rgb(var(--color-accent-hover) / <alpha-value>)',
        },
        fg: 'rgb(var(--color-fg) / <alpha-value>)',
        muted: 'rgb(var(--color-muted) / <alpha-value>)',
        dim: 'rgb(var(--color-dim) / <alpha-value>)',
        rule: 'rgb(var(--color-rule) / <alpha-value>)',
        sharp: 'rgb(var(--color-sharp) / <alpha-value>)',
      },
      fontFamily: {
        display: ['Fraunces', 'Georgia', 'serif'],
        sans: ['Geist', 'system-ui', 'sans-serif'],
        mono: ['Geist Mono', 'ui-monospace', 'monospace'],
      },
    },
  },
  plugins: [],
}
