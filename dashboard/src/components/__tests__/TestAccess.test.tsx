import { describe, expect, it } from 'vitest'
import { accessStatusFrom } from '../OnboardingWizard'
import { hasLegacyGitHubToken } from '../admin/IntegrationConfig'

// The wizard showed "Access OK" for any HTTP 200 and ignored the body; the
// backend always answers 200 with accessible:true/false (F21).
describe('accessStatusFrom', () => {
  it('reports the body, not the status code', () => {
    expect(accessStatusFrom({ accessible: true, status_code: 200 })).toEqual({ status: 'ok', msg: 'Access verified' })
    expect(accessStatusFrom({ accessible: false, error: 'repository has no GitHub remote; access is not checked through the GitHub API', remote_kind: 'local_path' }))
      .toEqual({ status: 'error', msg: 'repository has no GitHub remote; access is not checked through the GitHub API' })
    expect(accessStatusFrom({ accessible: false, status_code: 404 })).toEqual({ status: 'error', msg: 'GitHub answered 404' })
    expect(accessStatusFrom(null)).toEqual({ status: 'error', msg: 'No response' })
  })
})

// /admin/credentials masks every secret, so a check on the value could
// never see a legacy token; existence of the row is the signal (F9).
describe('hasLegacyGitHubToken', () => {
  it('finds a global github token row by name regardless of masking', () => {
    expect(hasLegacyGitHubToken([{ scope: 'global', category: 'github', key_name: 'token', value: '••••••••' }])).toBe(true)
    expect(hasLegacyGitHubToken([{ scope: 'global', category: 'github', key_name: 'github token', value: '••••••••' }])).toBe(true)
    expect(hasLegacyGitHubToken([{ scope: 'github_account', category: 'github', key_name: 'token', value: '••••••••' }])).toBe(false)
    expect(hasLegacyGitHubToken([{ scope: 'global', category: 'github', key_name: 'webhook_secret', value: '' }])).toBe(false)
  })
})
