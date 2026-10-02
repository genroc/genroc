package main

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"gopkg.in/yaml.v3"
)

type genrocConfig struct {
	Server string `yaml:"server,omitempty"`
	// Token is an API credential (genroc_sk_*). $GENROC_TOKEN wins over it.
	Token string `yaml:"token,omitempty"`
}

// configDir is $XDG_CONFIG_HOME/genroc, or ~/.config/genroc. NOT os.UserConfigDir: on macOS
// that is ~/Library/Application Support, and it ignores XDG_CONFIG_HOME.
func configDir() (string, error) {
	if x := os.Getenv("XDG_CONFIG_HOME"); x != "" {
		return filepath.Join(x, "genroc"), nil
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(home, ".config", "genroc"), nil
}

func configFilePath() (string, error) {
	dir, err := configDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(dir, "config.yaml"), nil
}

// legacyConfigFilePath is read-only: a config there is used until the next `config set`
// rewrites it in the new location.
func legacyConfigFilePath() (string, error) {
	dir, err := os.UserConfigDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(dir, "genroc", "config.yaml"), nil
}

func loadConfig() genrocConfig {
	data, err := readFirst(configFilePath, legacyConfigFilePath)
	if err != nil {
		return genrocConfig{}
	}
	var cfg genrocConfig
	yaml.Unmarshal(data, &cfg)
	return cfg
}

func readFirst(paths ...func() (string, error)) ([]byte, error) {
	var err error
	for _, p := range paths {
		path, perr := p()
		if perr != nil {
			err = perr
			continue
		}
		data, rerr := os.ReadFile(path)
		if rerr == nil {
			return data, nil
		}
		err = rerr
	}
	return nil, err
}

func saveConfig(cfg genrocConfig) error {
	path, err := configFilePath()
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
		return err
	}
	data, err := yaml.Marshal(cfg)
	if err != nil {
		return err
	}
	return os.WriteFile(path, data, 0600)
}

// ── last-instance state (genctl run → @last) ───────────────────────────────────

func lastInstanceFilePath() (string, error) {
	dir, err := configDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(dir, "last"), nil
}

func saveLastInstance(id string) error {
	path, err := lastInstanceFilePath()
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
		return err
	}
	return os.WriteFile(path, []byte(id+"\n"), 0600)
}

func loadLastInstance() string {
	path, err := lastInstanceFilePath()
	if err != nil {
		return ""
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(data))
}

// resolveInstanceID maps an instance-id argument to a concrete id: "@last" → the last
// started instance, else the value unchanged. Empty, or "@last" with none recorded, is fatal.
func resolveInstanceID(arg string) string {
	if arg == "" {
		fatal("an instance id is required — pass one explicitly, or @last for the most recently started instance")
	}
	if !isInstanceRef(arg) {
		// Checked here, not left to the server, whose "not found" reads as "it is gone" rather
		// than "that was never an id".
		fatal("not an instance id: %q — an id is an opaque digit-led token, or @last", arg)
	}
	if arg != "@last" {
		return arg
	}
	id := loadLastInstance()
	if id == "" {
		fatal("@last: no instance recorded yet — run `genctl run <process>` first")
	}
	return id
}

func runConfigCmd(args []string) {
	if len(args) > 0 && hasHelpArg(args[:1]) {
		helpFor("config")
		return
	}
	if len(args) < 2 {
		missingSubcommand("config")
	}
	sub, key := args[0], args[1]
	switch sub {
	case "get":
		cfg := loadConfig()
		val, err := configValue(cfg, key)
		if err != nil {
			fatal("%v", err)
		}
		if val == "" {
			fmt.Println("(not set)")
			return
		}
		// A credential is never printed back: whoever owns the file can read it there.
		if key == "token" {
			fmt.Printf("(set: %s)\n", maskToken(val))
			return
		}
		fmt.Println(val)
	case "set":
		if len(args) < 3 {
			fatal("usage: genctl config set <key> <value>")
		}
		val := args[2]
		cfg := loadConfig()
		switch key {
		case "server":
			cfg.Server = val
		case "token":
			cfg.Token = val
		default:
			fatal("unknown config key %q (server, token)", key)
		}
		if err := saveConfig(cfg); err != nil {
			fatal("save config: %v", err)
		}
		path, _ := configFilePath()
		shown := val
		if key == "token" {
			shown = maskToken(val)
		}
		fmt.Printf("set %s = %s  (%s)\n", key, shown, path)
	case "unset":
		cfg := loadConfig()
		switch key {
		case "server":
			cfg.Server = ""
		case "token":
			cfg.Token = ""
		default:
			fatal("unknown config key %q (server, token)", key)
		}
		if err := saveConfig(cfg); err != nil {
			fatal("save config: %v", err)
		}
		fmt.Printf("unset %s\n", key)
	default:
		fatal("unknown config subcommand %q (get, set, unset)", sub)
	}
}

func configValue(cfg genrocConfig, key string) (string, error) {
	switch key {
	case "server":
		return cfg.Server, nil
	case "token":
		return cfg.Token, nil
	}
	return "", fmt.Errorf("unknown config key %q (server, token)", key)
}
