package constant

import "testing"

// 这些数值是持久化到数据库的 ABI。改它们必须配一次数据迁移，
// 所以这里逐个钉住：重编号 = 测试失败 = 提醒你去写迁移。
func TestPersistenceEnumValues(t *testing.T) {
	cases := []struct {
		name string
		got  int
		want int
	}{
		{"RoleUser", RoleUser, 1},
		{"RoleAdmin", RoleAdmin, 10},
		{"RoleRoot", RoleRoot, 100},
		{"UserStatusEnabled", UserStatusEnabled, 1},
		{"UserStatusDisabled", UserStatusDisabled, 2},
		{"ChannelStatusEnabled", ChannelStatusEnabled, 1},
		{"ChannelStatusManuallyDisabled", ChannelStatusManuallyDisabled, 2},
		{"ChannelStatusAutoDisabled", ChannelStatusAutoDisabled, 3},
		{"TokenStatusEnabled", TokenStatusEnabled, 1},
		{"TokenStatusDisabled", TokenStatusDisabled, 2},
		{"TokenStatusExpired", TokenStatusExpired, 3},
		{"TokenStatusExhausted", TokenStatusExhausted, 4},
		{"LogTypeUnknown", LogTypeUnknown, 0},
		{"LogTypeConsume", LogTypeConsume, 1},
		{"LogTypeTopup", LogTypeTopup, 2},
		{"LogTypeManage", LogTypeManage, 3},
		{"LogTypeSystem", LogTypeSystem, 4},
		{"LogTypeError", LogTypeError, 5},
		{"LogTypeRefund", LogTypeRefund, 6},
		{"LogTypeLogin", LogTypeLogin, 7},
	}
	for _, c := range cases {
		if c.got != c.want {
			t.Errorf("%s = %d，但已发布的 ABI 值是 %d；"+
				"改变持久化枚举必须配一次数据迁移，不能只改常量", c.name, c.got, c.want)
		}
	}
}

// 同一枚举内撞值是最容易犯的重排错误：两个不同的语义落库后无法区分。
func TestEnumsHaveNoDuplicateValues(t *testing.T) {
	groups := map[string]map[string]int{
		"UserStatus": {
			"Enabled":  UserStatusEnabled,
			"Disabled": UserStatusDisabled,
		},
		"ChannelStatus": {
			"Enabled":          ChannelStatusEnabled,
			"ManuallyDisabled": ChannelStatusManuallyDisabled,
			"AutoDisabled":     ChannelStatusAutoDisabled,
		},
		"TokenStatus": {
			"Enabled":   TokenStatusEnabled,
			"Disabled":  TokenStatusDisabled,
			"Expired":   TokenStatusExpired,
			"Exhausted": TokenStatusExhausted,
		},
		"LogType": {
			"Unknown": LogTypeUnknown,
			"Consume": LogTypeConsume,
			"Topup":   LogTypeTopup,
			"Manage":  LogTypeManage,
			"System":  LogTypeSystem,
			"Error":   LogTypeError,
			"Refund":  LogTypeRefund,
			"Login":   LogTypeLogin,
		},
	}
	for group, members := range groups {
		seen := map[int]string{}
		for name, v := range members {
			if dup, ok := seen[v]; ok {
				t.Errorf("%s.%s 与 %s 撞值 %d", group, name, dup, v)
			}
			seen[v] = name
		}
	}
}
