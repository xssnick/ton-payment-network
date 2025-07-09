//go:build js && wasm

package config

import (
	"encoding/json"
	"syscall/js"
)

func setLocalStorage(key, value string) {
	js.Global().Get("localStorage").Call("setItem", key, value)
}

func getLocalStorage(key string) string {
	v := js.Global().Get("localStorage").Call("getItem", key)
	if v.IsNull() || v.IsUndefined() {
		return ""
	}
	return v.String()
}

func LoadConfig(path string) (*Config, error) {
	res := getLocalStorage(path)

	if res == "" {
		cfg, err := Generate()
		if err != nil {
			return nil, err
		}

		err = SaveConfig(cfg, path)
		if err != nil {
			return nil, err
		}

		return cfg, nil
	}

	var cfg Config
	if err := json.Unmarshal([]byte(res), &cfg); err != nil {
		return nil, err
	}

	cfg.ChannelConfig.SupportedCoins.Ton.Decimals = 9 // force to avoid problems on change
	return &cfg, nil
}

func SaveConfig(cfg *Config, path string) error {
	data, err := json.Marshal(cfg)
	if err != nil {
		return err
	}

	setLocalStorage(path, string(data))
	return nil
}
