import { useCallback, useMemo } from 'react'

const BASE_URL = '/api'
const TOKEN_KEY = 'auth_token'

interface LoginResponse {
  token: string
}

function getToken(): string | null {
  return localStorage.getItem(TOKEN_KEY)
}

function buildHeaders(extra?: Record<string, string>): Record<string, string> {
  const headers: Record<string, string> = {
    'Content-Type': 'application/json',
    ...extra,
  }
  const token = getToken()
  if (token) {
    headers['Authorization'] = `Bearer ${token}`
  }
  return headers
}

async function request<T>(
  method: string,
  path: string,
  body?: unknown,
): Promise<T> {
  const res = await fetch(`${BASE_URL}${path}`, {
    method,
    headers: buildHeaders(),
    body: body !== undefined ? JSON.stringify(body) : undefined,
  })

  if (!res.ok) {
    if (res.status === 401) {
      localStorage.removeItem(TOKEN_KEY)
      window.location.href = '/login'
      throw new Error('Session expired')
    }
    const text = await res.text().catch(() => res.statusText)
    throw new Error(`API ${method} ${path} → ${res.status}: ${text}`)
  }

  // handle 204 No Content
  if (res.status === 204) {
    return undefined as unknown as T
  }

  return res.json() as Promise<T>
}

export interface UseApiReturn {
  get: <T>(path: string) => Promise<T>
  post: <T>(path: string, body?: unknown) => Promise<T>
  put: <T>(path: string, body?: unknown) => Promise<T>
  patch: <T>(path: string, body?: unknown) => Promise<T>
  del: <T>(path: string) => Promise<T>
  login: (email: string, password: string) => Promise<void>
  logout: () => void
  isAuthenticated: () => boolean
}

export function useApi(): UseApiReturn {
  const get = useCallback(<T>(path: string) => request<T>('GET', path), [])
  const post = useCallback(
    <T>(path: string, body?: unknown) => request<T>('POST', path, body),
    [],
  )
  const put = useCallback(
    <T>(path: string, body?: unknown) => request<T>('PUT', path, body),
    [],
  )
  const patch = useCallback(
    <T>(path: string, body?: unknown) => request<T>('PATCH', path, body),
    [],
  )
  const del = useCallback(<T>(path: string) => request<T>('DELETE', path), [])

  const login = useCallback(async (email: string, password: string) => {
    const res = await request<LoginResponse>('POST', '/auth/login', {
      email,
      password,
    })
    localStorage.setItem(TOKEN_KEY, res.token)
  }, [])

  const logout = useCallback(() => {
    localStorage.removeItem(TOKEN_KEY)
  }, [])

  const isAuthenticated = useCallback(() => Boolean(getToken()), [])

  return useMemo(
    () => ({ get, post, put, patch, del, login, logout, isAuthenticated }),
    [get, post, put, patch, del, login, logout, isAuthenticated],
  )
}
