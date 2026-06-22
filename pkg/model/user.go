package model

import "strings"

// UserRole is a platform-level role flag on the User record.
// Empty Roles reads as ["user"]; only positive marker is "admin".
type UserRole string

const (
	UserRoleAdmin UserRole = "admin"
	UserRoleUser  UserRole = "user"
)

type User struct {
	ID           string     `json:"id"           bson:"_id"`
	Account      string     `json:"account"      bson:"account"`
	SiteID       string     `json:"siteId"       bson:"siteId"`
	SectID       string     `json:"sectId"       bson:"sectId"`
	SectName     string     `json:"sectName"     bson:"sectName"`
	SectTCName   string     `json:"sectTCName"   bson:"sectTCName"`
	DeptID       string     `json:"deptId"       bson:"deptId"`
	DeptName     string     `json:"deptName"     bson:"deptName"`
	DeptTCName   string     `json:"deptTCName"   bson:"deptTCName"`
	EngName      string     `json:"engName"      bson:"engName"`
	ChineseName  string     `json:"chineseName"  bson:"chineseName"`
	EmployeeID   string     `json:"employeeId"   bson:"employeeId"`
	StatusIsShow bool       `json:"statusIsShow" bson:"statusIsShow"`
	StatusText   string     `json:"statusText"   bson:"statusText"`
	Roles        []UserRole `json:"roles,omitempty"        bson:"roles,omitempty"`
}

// IsPlatformAdmin reports whether u holds the platform admin role. Nil-safe.
func IsPlatformAdmin(u *User) bool {
	if u == nil {
		return false
	}
	for _, r := range u.Roles {
		if r == UserRoleAdmin {
			return true
		}
	}
	return false
}

// IsPlatformAdminAccount reports whether account is a platform-admin account
// (a "p_" prefix).
func IsPlatformAdminAccount(account string) bool {
	return strings.HasPrefix(account, "p_")
}

// IsBot reports whether account is a bot account (a ".bot" suffix).
func IsBot(account string) bool {
	return strings.HasSuffix(account, ".bot")
}

// DisplayName renders the user's display label for Drive ownership metadata:
// the account when either name is missing, the English name when both names are
// identical, otherwise "<engName> <chineseName>".
func (u *User) DisplayName() string {
	switch {
	case u == nil:
		return ""
	case u.EngName == "" || u.ChineseName == "":
		return u.Account
	case u.EngName == u.ChineseName:
		return u.EngName
	default:
		return u.EngName + " " + u.ChineseName
	}
}
