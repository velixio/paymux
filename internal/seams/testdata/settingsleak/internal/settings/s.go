package settings

import "example.com/fix/internal/store"

func f() { _, _, _ = store.GetAppSetting(nil, nil, "a", "k") }
