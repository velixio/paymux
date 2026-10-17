package config

import "log/slog"

const redacted = "[redacted]"

// Secret is a string that never prints itself: formatting, slog and JSON all
// show "[redacted]". Use Reveal where the value is actually needed.
type Secret string

// Reveal returns the secret value.
func (s Secret) Reveal() string { return string(s) }

func (Secret) String() string               { return redacted }
func (Secret) GoString() string             { return `config.Secret("` + redacted + `")` }
func (Secret) MarshalText() ([]byte, error) { return []byte(redacted), nil }
func (Secret) LogValue() slog.Value         { return slog.StringValue(redacted) }
