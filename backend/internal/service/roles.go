package service

func IsValidUserRole(role string) bool {
	switch role {
	case RoleAdmin, RoleAccountManager, RoleUser:
		return true
	default:
		return false
	}
}

func IsAssignableUserRole(role string) bool {
	switch role {
	case RoleAccountManager, RoleUser:
		return true
	default:
		return false
	}
}
