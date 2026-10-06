// vim: set ts=4 sw=4 noet:
package main

import (
	"fmt"
	"os"
	"strconv"
	"strings"
	"time"
)

// Config holds every setting read from the environment. It is parsed once at
// startup so a typo in a variable aborts the service instead of being
// silently ignored in the middle of an import.
type Config struct {
	DataDir  string
	Port     string
	Token    string
	Location *time.Location

	// Indexing options. They decide which (partial) personal data is stored,
	// so the defaults are the most privacy preserving ones.
	DocumentChars       int // 0 keeps the whole document
	FirstChars          bool
	FirstCharsAddLetter bool
	NameChars           int
	Day                 bool
	Year                bool
	Fn                  bool
	Sn1                 bool
	Sn2                 bool
	PostCode            bool
}

func loadConfig() (Config, error) {
	cfg := Config{
		DataDir: envString("DATA_DIR", "/data"),
		Port:    envString("PORT", "8080"),
		Token:   os.Getenv("TOKEN"),
	}

	if cfg.Token == "" {
		return cfg, fmt.Errorf("TOKEN is not set")
	}

	var err error
	if cfg.Location, err = time.LoadLocation(envString("TIMEZONE", "UTC")); err != nil {
		return cfg, fmt.Errorf("TIMEZONE: %w", err)
	}

	if cfg.DocumentChars, err = envInt("DOCUMENT_CHARS", 5); err != nil {
		return cfg, err
	}
	if cfg.DocumentChars != 0 && cfg.DocumentChars < 3 {
		return cfg, fmt.Errorf("DOCUMENT_CHARS must be 0 (whole document) or at least 3, got %d", cfg.DocumentChars)
	}
	if cfg.NameChars, err = envInt("NAME_CHARS", 2); err != nil {
		return cfg, err
	}
	if cfg.NameChars < 1 {
		return cfg, fmt.Errorf("NAME_CHARS must be at least 1, got %d", cfg.NameChars)
	}

	bools := []struct {
		name string
		dst  *bool
	}{
		{"FIRST_CHARS", &cfg.FirstChars},
		{"FIRST_CHARS_ADD_LETTER", &cfg.FirstCharsAddLetter},
		{"DAY", &cfg.Day},
		{"YEAR", &cfg.Year},
		{"FN", &cfg.Fn},
		{"SN1", &cfg.Sn1},
		{"SN2", &cfg.Sn2},
		{"POST_CODE", &cfg.PostCode},
	}
	for _, b := range bools {
		if *b.dst, err = envBool(b.name, false); err != nil {
			return cfg, err
		}
	}

	return cfg, nil
}

func envString(name, def string) string {
	if v := strings.TrimSpace(os.Getenv(name)); v != "" {
		return v
	}
	return def
}

func envInt(name string, def int) (int, error) {
	v := strings.TrimSpace(os.Getenv(name))
	if v == "" {
		return def, nil
	}
	n, err := strconv.Atoi(v)
	if err != nil {
		return 0, fmt.Errorf("%s: invalid integer %q", name, v)
	}
	return n, nil
}

func envBool(name string, def bool) (bool, error) {
	v := strings.TrimSpace(os.Getenv(name))
	if v == "" {
		return def, nil
	}
	b, err := strconv.ParseBool(v)
	if err != nil {
		return false, fmt.Errorf("%s: invalid boolean %q", name, v)
	}
	return b, nil
}
