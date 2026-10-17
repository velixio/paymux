package config

import (
	"bufio"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"strings"
)

// LoadDotEnv parses a .env file: KEY=VALUE lines, # comments, optional
// "export " prefix, single or double quotes and CRLF endings. A missing file
// yields an empty map and no error.
func LoadDotEnv(path string) (map[string]string, error) {
	f, err := os.Open(path)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return map[string]string{}, nil
		}
		return nil, fmt.Errorf("open %s: %w", path, err)
	}
	defer f.Close()

	out := map[string]string{}
	sc := bufio.NewScanner(f)
	n := 0
	for sc.Scan() {
		n++
		line := strings.TrimSpace(strings.TrimSuffix(sc.Text(), "\r"))
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		line = strings.TrimSpace(strings.TrimPrefix(line, "export "))
		key, raw, ok := strings.Cut(line, "=")
		key = strings.TrimSpace(key)
		if !ok || key == "" {
			return nil, fmt.Errorf("%s: line %d: expected KEY=VALUE", path, n)
		}
		val, err := parseValue(strings.TrimSpace(raw))
		if err != nil {
			return nil, fmt.Errorf("%s: line %d: %w", path, n, err)
		}
		out[key] = val
	}
	if err := sc.Err(); err != nil {
		return nil, fmt.Errorf("read %s: %w", path, err)
	}
	return out, nil
}

func parseValue(raw string) (string, error) {
	if raw != "" && (raw[0] == '"' || raw[0] == '\'') {
		end := strings.IndexByte(raw[1:], raw[0])
		if end < 0 {
			return "", errors.New("unterminated quote")
		}
		rest := strings.TrimSpace(raw[end+2:])
		if rest != "" && !strings.HasPrefix(rest, "#") {
			return "", errors.New("unexpected text after closing quote")
		}
		return raw[1 : end+1], nil
	}
	for i := 1; i < len(raw); i++ {
		if raw[i] == '#' && (raw[i-1] == ' ' || raw[i-1] == '\t') {
			raw = raw[:i]
			break
		}
	}
	return strings.TrimSpace(raw), nil
}

// WithDotEnv layers file values under the real environment: the real
// environment wins.
func WithDotEnv(lookupEnv func(string) (string, bool), file map[string]string) func(string) (string, bool) {
	return func(k string) (string, bool) {
		if v, ok := lookupEnv(k); ok {
			return v, true
		}
		v, ok := file[k]
		return v, ok
	}
}
