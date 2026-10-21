package model

import (
	"errors"
	"testing"

	"github.com/yyyCode/whats-api/internal/constant"
)

func TestGetTokenByKey(t *testing.T) {
	withTestDB(t)

	tok := Token{Key: "sk-abc", UserID: 1, Status: constant.TokenStatusEnabled, ExpiredTime: -1}
	if err := DB.Create(&tok).Error; err != nil {
		t.Fatal(err)
	}

	got, err := GetTokenByKey("sk-abc")
	if err != nil {
		t.Fatalf("查询失败: %v", err)
	}
	if got.ID != tok.ID {
		t.Errorf("ID = %d，期望 %d", got.ID, tok.ID)
	}

	if _, err := GetTokenByKey("sk-none"); !errors.Is(err, ErrNotFound) {
		t.Errorf("不存在的 key 应返回 ErrNotFound，实际 %v", err)
	}
}

func TestGetUserByID(t *testing.T) {
	withTestDB(t)

	u := User{Username: "u1", GroupName: "default", Status: constant.UserStatusEnabled}
	if err := DB.Create(&u).Error; err != nil {
		t.Fatal(err)
	}
	got, err := GetUserByID(u.ID)
	if err != nil || got.Username != "u1" {
		t.Fatalf("got=%+v err=%v", got, err)
	}
	if _, err := GetUserByID(9999); !errors.Is(err, ErrNotFound) {
		t.Errorf("不存在的用户应返回 ErrNotFound，实际 %v", err)
	}
}

func TestDisableChannelOnlyTouchesEnabled(t *testing.T) {
	withTestDB(t)

	enabled := Channel{Type: 1, Status: constant.ChannelStatusEnabled}
	manual := Channel{Type: 1, Status: constant.ChannelStatusManuallyDisabled}
	if err := DB.Create(&enabled).Error; err != nil {
		t.Fatal(err)
	}
	if err := DB.Create(&manual).Error; err != nil {
		t.Fatal(err)
	}

	ok, err := DisableChannel(enabled.ID)
	if err != nil || !ok {
		t.Fatalf("禁用启用中的渠道: ok=%v err=%v", ok, err)
	}
	var got Channel
	DB.First(&got, enabled.ID)
	if got.Status != constant.ChannelStatusAutoDisabled {
		t.Errorf("status = %d，期望 auto_disabled", got.Status)
	}

	ok, err = DisableChannel(manual.ID)
	if err != nil || ok {
		t.Fatalf("手动禁用的渠道不该被改写: ok=%v err=%v", ok, err)
	}
	// 复用 got 时它仍带着上一行的主键，GORM 会拼出 id = manual.ID AND id = enabled.ID；
	// 清零后再查，确保读到的确实是 manual 这一行。
	got = Channel{}
	DB.First(&got, manual.ID)
	if got.Status != constant.ChannelStatusManuallyDisabled {
		t.Errorf("手动禁用态被改成了 %d", got.Status)
	}
}

func TestGetEnabledChannels(t *testing.T) {
	withTestDB(t)

	DB.Create(&Channel{Type: 1, Status: constant.ChannelStatusEnabled})
	DB.Create(&Channel{Type: 1, Status: constant.ChannelStatusManuallyDisabled})

	got, err := GetEnabledChannels()
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 {
		t.Errorf("只应返回启用的渠道，实际 %d 条", len(got))
	}
}
