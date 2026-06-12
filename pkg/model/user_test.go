package model

import "testing"

func TestUser_DisplayName(t *testing.T) {
	tests := []struct {
		name string
		user *User
		want string
	}{
		{"nil user", nil, ""},
		{"both names empty -> account", &User{Account: "alice", EngName: "", ChineseName: ""}, "alice"},
		{"eng empty -> account", &User{Account: "alice", EngName: "", ChineseName: "陳"}, "alice"},
		{"chinese empty -> account", &User{Account: "alice", EngName: "Alice", ChineseName: ""}, "alice"},
		{"equal names -> eng", &User{Account: "alice", EngName: "Same", ChineseName: "Same"}, "Same"},
		{"distinct names -> joined", &User{Account: "alice", EngName: "Alice", ChineseName: "陳"}, "Alice 陳"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := tc.user.DisplayName(); got != tc.want {
				t.Fatalf("DisplayName() = %q, want %q", got, tc.want)
			}
		})
	}
}
