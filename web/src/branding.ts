import type { Profile } from './api-types'

export const defaultLogo = '/logo.svg'

export function updateDocumentBranding(profile: Profile) {
  document.title = profile.projectName
  document.querySelector('link[rel="icon"]')?.setAttribute('href', profile.logo || defaultLogo)
}
