// M4-06 default-setting guard, client side. A configuration change that
// alters the production configuration (the default agent definition, any
// enabled definition, prompt templates, the default provider's model)
// without an approved rollout is refused by the backend with
// 409 rollout_required. The admin may retry with an override reason, which
// the backend writes to the rollout_overrides audit table.

export const OVERRIDE_PROMPT =
  'This change alters the production configuration and no approved rollout covers it ' +
  '(Admin → Rollouts). Enter an override reason to apply it anyway — it is recorded in the audit log. ' +
  'Cancel to leave the configuration unchanged.'

export function isRolloutRequired(e: unknown): boolean {
  return e instanceof Error && e.message.includes('→ 409') && e.message.includes('rollout_required')
}

export function withOverrideReason(path: string, reason: string): string {
  const sep = path.includes('?') ? '&' : '?'
  return `${path}${sep}override_reason=${encodeURIComponent(reason)}`
}

function defaultAsk(): string | null {
  return typeof window !== 'undefined' ? window.prompt(OVERRIDE_PROMPT) : null
}

// guarded runs call(path); on a rollout_required refusal it asks for an
// override reason and retries once with it. No reason → the refusal stands.
export async function guarded<T>(
  path: string,
  call: (p: string) => Promise<T>,
  ask: () => string | null = defaultAsk,
): Promise<T> {
  try {
    return await call(path)
  } catch (e) {
    if (!isRolloutRequired(e)) throw e
    const reason = (ask() ?? '').trim()
    if (!reason) {
      throw new Error('Change refused: it alters the production configuration without an approved rollout (Admin → Rollouts), and no override reason was given.')
    }
    return call(withOverrideReason(path, reason))
  }
}
