import type { RiskLevel } from '@/types'

/**
 * Locked risk palette — spec § Decisions locked, 2026-04-18.
 * Solid fills chosen for visibility against the dark `surface-800` bg.
 * Border matches the hue at 100%; fill is a 15% tint of the same hue.
 * External nodes render slate at 50% opacity (dimmed).
 */
export const RISK_COLORS: Record<RiskLevel, { border: string; fill: string; text: string }> = {
  low:     { border: '#10b981', fill: 'rgba(16, 185, 129, 0.15)', text: '#a7f3d0' },
  medium:  { border: '#f59e0b', fill: 'rgba(245, 158, 11, 0.15)', text: '#fcd34d' },
  high:    { border: '#ef4444', fill: 'rgba(239, 68, 68, 0.15)',  text: '#fca5a5' },
  unknown: { border: '#64748b', fill: 'rgba(100, 116, 139, 0.15)', text: '#cbd5e1' },
}

export const EXTERNAL_COLORS = {
  border: '#64748b',
  fill: 'rgba(100, 116, 139, 0.10)',
  text: '#94a3b8',
  opacity: 0.5,
}

export const ALL_RISKS: RiskLevel[] = ['low', 'medium', 'high', 'unknown']
