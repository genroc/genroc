const base = import.meta.env.BASE_URL

// Every href and asset path goes through here. An archived build is served from `/v1/`,
// where a root-absolute link silently resolves to the latest version instead.
export function url(path: string): string {
  return `${base.replace(/\/$/, '')}/${path.replace(/^\//, '')}`.replace(/\/?$/, '/')
}

export function asset(path: string): string {
  return `${base.replace(/\/$/, '')}/${path.replace(/^\//, '')}`
}
