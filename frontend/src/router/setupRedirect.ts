export function resolveCompletedSetupRedirectPath(isAuthenticated: boolean, isAdmin: boolean, isAccountManager: boolean = false): string {
  if (!isAuthenticated) {
    return '/login'
  }

  if (isAccountManager && !isAdmin) {
    return '/admin/accounts'
  }

  return isAdmin ? '/admin/dashboard' : '/dashboard'
}
