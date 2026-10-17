package api

import "example.com/fix/internal/store"

var _ = store.GetAppSetting

func f() { _, _, _ = store.GetAppSetting(nil, nil, "a", "k") }
