package config

import (
	"crypto/ed25519"
	"crypto/rand"
	"encoding/base64"
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
	WebTransportListenAddr         string
	MetricsListenAddr              string
	MetricsNamespace               string
	WebhooksSignatureHMACSHA256Key string
	NodeListenAddr                 string
	ExternalIP                     string
	NetworkConfigUrl               string
	DBPath                         string
	SecureProofPolicy              bool
	ChannelConfig                  ChannelsConfig
}

func Generate() (*Config, error) {
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
		WebTransportListenAddr:         "",
		MetricsListenAddr:              "0.0.0.0:8097",
		MetricsNamespace:               "",
		NodeListenAddr:                 "0.0.0.0:17555",
		ExternalIP:                     "",
		NetworkConfigUrl:               "https://ton-blockchain.github.io/global.config.json",
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

	return cfg, nil
}
