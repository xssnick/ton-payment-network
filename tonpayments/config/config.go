package config

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"github.com/rs/zerolog/log"
	"net"
	"os"
	"path/filepath"
	"time"
)

type VirtualConfig struct {
	ProxyMaxCapacity string
	ProxyMinFee      string
	ProxyFeePercent  float64
	AllowTunneling   bool
}

type BalanceControlConfig struct {
	DepositWhenAmountLessThan string
	DepositUpToAmount         string
	WithdrawWhenAmountReached string
}

type CoinConfig struct {
	Enabled             bool
	VirtualTunnelConfig VirtualConfig
	MisbehaviorFine     string
	ExcessFeeTon        string
	Symbol              string
	Decimals            uint8

	BalanceControl *BalanceControlConfig
}

type ChannelsConfig struct {
	SupportedCoins CoinTypes

	BufferTimeToCommit              uint32
	QuarantineDurationSec           uint32
	ConditionalCloseDurationSec     uint32
	MinSafeVirtualChannelTimeoutSec uint32
}

type CoinTypes struct {
	Ton             CoinConfig
	Jettons         map[string]CoinConfig
	ExtraCurrencies map[uint32]CoinConfig
}

type Config struct {
	ADNLServerKey                  []byte
	PaymentNodePrivateKey          []byte
	WalletPrivateKey               []byte
	APIListenAddr                  string
	WebhooksSignatureHMACSHA256Key string
	NodeListenAddr                 string
	ExternalIP                     string
	NetworkConfigUrl               string
	DBPath                         string
	SecureProofPolicy              bool
	ChannelConfig                  ChannelsConfig
}

func checkIPAddress(ip string) string {
	p := net.ParseIP(ip)
	if p == nil {
		log.Warn().Int("len", len(p)).Msg("bad ip")
		return ""
	}
	p = p.To4()
	if p == nil {
		log.Warn().Int("len", len(p)).Msg("bad ip, not v4")
		return ""
	}

	return p.String()
}

func checkCanSeed() (string, bool) {
	ch := make(chan bool, 1)

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	ip := ""
	go func() {
		defer func() {
			ch <- ip != ""
		}()

		listen, err := net.Listen("tcp", "0.0.0.0:18889")
		if err != nil {
			log.Error().Err(err).Str("source", "port checker").Msg("listen err")
			return
		}
		defer listen.Close()

		conn, err := listen.Accept()
		if err != nil {
			log.Error().Err(err).Str("source", "port checker").Msg("accept err")
			return
		}

		ipData := make([]byte, 256)
		n, err := conn.Read(ipData)
		if err != nil {
			log.Error().Err(err).Str("source", "port checker").Msg("read err")
			return
		}

		ip = string(ipData[:n])
		ip = checkIPAddress(ip)
		_ = conn.Close()
	}()

	log.Info().Msg("resolving port checker...")

	ips, err := net.LookupIP("tonutils.com")
	if err != nil || len(ips) == 0 {
		log.Warn().Msg("port checker is not resolved, if you have white ip and open ports, please specify your external ip manually in config.json")
		return "", false
	}
	log.Info().Msg("port checker resolved, using port checker at tonutils.com")

	conn, err := net.Dial("tcp", ips[0].String()+":9099")
	if err != nil {
		return "", false
	}

	_, err = conn.Write([]byte("ME"))
	if err != nil {
		return "", false
	}
	ok := false
	select {
	case k := <-ch:
		log.Info().Str("ip", ip).Msg("ports are open, your payment node is available from internet, anyone can reach you")

		ok = k
	case <-ctx.Done():
		log.Warn().Msg("no request from port checker, looks like it cannot reach you, so ports are probably closed, only you can initiate connections to other nodes. If it is a mistake, just specify your external ip in config.json")
	}

	return ip, ok
}

func LoadConfig(path string) (*Config, error) {
	path, err := filepath.Abs(path)
	if err != nil {
		return nil, err
	}

	dir := filepath.Dir(path)
	_, err = os.Stat(dir)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			err = os.MkdirAll(dir, os.ModePerm)
		}
		if err != nil {
			return nil, fmt.Errorf("failed to check directory: %w", err)
		}
	}

	_, err = os.Stat(path)
	if os.IsNotExist(err) {
		_, priv, err := ed25519.GenerateKey(nil)
		if err != nil {
			return nil, err
		}

		_, walletPriv, err := ed25519.GenerateKey(nil)
		if err != nil {
			return nil, err
		}

		_, nodePriv, err := ed25519.GenerateKey(nil)
		if err != nil {
			return nil, err
		}

		whKey := make([]byte, 32)
		if _, err = rand.Read(whKey); err != nil {
			return nil, err
		}

		cfg := &Config{
			ADNLServerKey:                  nodePriv.Seed(),
			PaymentNodePrivateKey:          priv.Seed(),
			WalletPrivateKey:               walletPriv.Seed(),
			APIListenAddr:                  "0.0.0.0:8096",
			NodeListenAddr:                 "0.0.0.0:17555",
			ExternalIP:                     "",
			NetworkConfigUrl:               "https://ton.org/testnet-global.config.json",
			DBPath:                         "./payment-node-db",
			WebhooksSignatureHMACSHA256Key: base64.StdEncoding.EncodeToString(whKey),
			SecureProofPolicy:              false,
			ChannelConfig: ChannelsConfig{
				SupportedCoins: CoinTypes{
					Ton: CoinConfig{
						Enabled: true,
						VirtualTunnelConfig: VirtualConfig{
							ProxyMaxCapacity: "5",
							ProxyMinFee:      "0.0005",
							ProxyFeePercent:  0.5,
							AllowTunneling:   true,
						},
						BalanceControl: &BalanceControlConfig{
							DepositWhenAmountLessThan: "2",
							DepositUpToAmount:         "3",
							WithdrawWhenAmountReached: "5",
						},
						MisbehaviorFine: "3",
						ExcessFeeTon:    "0.25",
						Symbol:          "TON",
						Decimals:        9,
					},
					Jettons: map[string]CoinConfig{
						"EQCxE6mUtQJKFnGfaROTKOt1lZbDiiX1kCixRv7Nw2Id_sDs": {
							Enabled: false,
							VirtualTunnelConfig: VirtualConfig{
								ProxyMaxCapacity: "15.5",
								ProxyMinFee:      "0.002",
								ProxyFeePercent:  0.8,
								AllowTunneling:   false,
							},
							MisbehaviorFine: "12",
							ExcessFeeTon:    "0.35",
							Symbol:          "USDT",
							Decimals:        6,
						},
					},
					ExtraCurrencies: map[uint32]CoinConfig{},
				},
				BufferTimeToCommit:              3 * 3600,
				QuarantineDurationSec:           6 * 3600,
				ConditionalCloseDurationSec:     3 * 3600,
				MinSafeVirtualChannelTimeoutSec: 60,
			},
		}

		ip, seed := checkCanSeed()
		if seed {
			cfg.ExternalIP = ip
		}

		err = SaveConfig(cfg, path)
		if err != nil {
			return nil, err
		}

		return cfg, nil
	} else if err == nil {
		data, err := os.ReadFile(path)
		if err != nil {
			return nil, err
		}

		var cfg Config
		err = json.Unmarshal(data, &cfg)
		if err != nil {
			return nil, err
		}
		cfg.ChannelConfig.SupportedCoins.Ton.Decimals = 9 // force to avoid problems on change
		return &cfg, nil
	}

	return nil, err
}

func SaveConfig(cfg *Config, path string) error {
	data, err := json.MarshalIndent(cfg, "", "\t")
	if err != nil {
		return err
	}

	err = os.WriteFile(path, data, 0766)
	if err != nil {
		return err
	}
	return nil
}
