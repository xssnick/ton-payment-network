package main

import (
	"context"
	"encoding/hex"
	"errors"
	"flag"
	"fmt"
	"github.com/rs/zerolog"
	"github.com/rs/zerolog/log"
	"github.com/xssnick/ton-payment-network/pkg/payments"
	"github.com/xssnick/ton-payment-network/tonpayments"
	"github.com/xssnick/ton-payment-network/tonpayments/api"
	"github.com/xssnick/ton-payment-network/tonpayments/chain"
	"github.com/xssnick/ton-payment-network/tonpayments/config"
	"github.com/xssnick/ton-payment-network/tonpayments/db"
	"github.com/xssnick/ton-payment-network/tonpayments/db/leveldb"
	"github.com/xssnick/ton-payment-network/tonpayments/transport"
	"github.com/xssnick/tonutils-go/address"
	"github.com/xssnick/tonutils-go/adnl"
	"github.com/xssnick/tonutils-go/adnl/dht"
	"github.com/xssnick/tonutils-go/liteclient"
	"github.com/xssnick/tonutils-go/tlb"
	"github.com/xssnick/tonutils-go/ton"
	"github.com/xssnick/tonutils-go/ton/wallet"
	"github.com/xssnick/tonutils-go/tvm/cell"
	"golang.org/x/crypto/ed25519"
	"math"
	"math/big"
	"net"
	"strconv"
	"strings"
	"time"
)

var Verbosity = flag.Int("v", 2, "verbosity")
var DaemonMode = flag.Bool("daemon", false, "daemon mode (disables command reader)")
var Webhook = flag.String("webhook", "", "HTTP webhook address")
var API = flag.String("api", "", "HTTP API listen address")
var APICredentialsLogin = flag.String("api-login", "", "HTTP API credentials login")
var APICredentialsPassword = flag.String("api-password", "", "HTTP API credentials password")
var ConfigPath = flag.String("config", "payment-network-config.json", "config path")
var ForceBlock = flag.Uint64("force-block", 0, "master block seqno to start scan from, ignored if 0, otherwise - overrides db value")
var UseBlockScanner = flag.Bool("use-block-scanner", false, "use block scanner instead of watching specific contracts")

func main() {
	flag.Parse()

	log.Logger = zerolog.New(zerolog.NewConsoleWriter()).With().Timestamp().Logger().Level(zerolog.InfoLevel)
	scanLog := log.Logger
	if *Verbosity >= 4 {
		scanLog = scanLog.Level(zerolog.DebugLevel).With().Logger()
	}

	if *Verbosity >= 5 {
		dht.Logger = func(v ...any) {
			log.Logger.Debug().Msg(fmt.Sprintln(v...))
		}
	}

	if *Verbosity >= 3 {
		log.Logger = log.Logger.Level(zerolog.DebugLevel).With().Logger()
	} else if *Verbosity == 2 {
		log.Logger = log.Logger.Level(zerolog.InfoLevel).With().Logger()
	} else if *Verbosity == 1 {
		log.Logger = log.Logger.Level(zerolog.WarnLevel).With().Logger()
	} else if *Verbosity == 0 {
		log.Logger = log.Logger.Level(zerolog.ErrorLevel).With().Logger()
	} else {
		log.Logger = log.Logger.Level(zerolog.FatalLevel).With().Logger()
	}

	adnl.Logger = func(v ...any) {}

	if *ConfigPath == "" {
		log.Fatal().Msg("-config should have value or be not presented")
		return
	}

	cfg, err := config.LoadConfig(*ConfigPath)
	if err != nil {
		log.Fatal().Err(err).Msg("failed to load config")
		return
	}

	log.Info().Msg("initializing ton client with verified proof chain...")

	client := liteclient.NewConnectionPool()

	tonCfg, err := liteclient.GetConfigFromUrl(context.Background(), cfg.NetworkConfigUrl)
	if err != nil {
		log.Fatal().Err(err).Msg("failed to get network config")
		return
	}

	// connect to lite servers
	err = client.AddConnectionsFromConfig(context.Background(), tonCfg)
	if err != nil {
		log.Fatal().Err(err).Msg("ton connect err")
		return
	}

	policy := ton.ProofCheckPolicyFast
	if cfg.SecureProofPolicy {
		policy = ton.ProofCheckPolicySecure
	}

	// initialize ton api lite connection wrapper
	apiClient := ton.NewAPIClient(client, policy).WithRetry(2).WithTimeout(5 * time.Second)
	if cfg.SecureProofPolicy {
		apiClient.SetTrustedBlockFromConfig(tonCfg)
	}

	_, dhtKey, err := ed25519.GenerateKey(nil)
	dhtGate := adnl.NewGateway(dhtKey)
	if err = dhtGate.StartClient(); err != nil {
		log.Fatal().Err(err).Msg("failed to init adnl gateway for dht")
		return
	}

	dhtClient, err := dht.NewClientFromConfig(dhtGate, tonCfg)
	if err != nil {
		log.Fatal().Err(err).Msg("failed to init dht client")
		return
	}

	gate := adnl.NewGateway(cfg.ADNLServerKey)

	if cfg.ExternalIP != "" {
		ip := net.ParseIP(cfg.ExternalIP)
		if ip == nil {
			log.Fatal().Msg("incorrect ip format")
			return
		}

		gate.SetExternalIP(ip.To4())
		if err := gate.StartServer(cfg.NodeListenAddr); err != nil {
			log.Fatal().Err(err).Msg("failed to init adnl gateway")
			return
		}
	} else {
		if err := gate.StartClient(); err != nil {
			log.Fatal().Err(err).Msg("failed to init adnl gateway")
			return
		}
	}

	fdb, err := leveldb.NewDB(cfg.DBPath, cfg.PaymentNodePrivateKey.Public().(ed25519.PublicKey))
	if err != nil {
		log.Fatal().Err(err).Msg("failed to init leveldb")
		return
	}

	tr := transport.NewServer(dhtClient, gate, cfg.ADNLServerKey, cfg.PaymentNodePrivateKey, cfg.ExternalIP != "")

	var seqno uint32
	if bo, err := fdb.GetBlockOffset(context.Background()); err != nil {
		if !errors.Is(err, db.ErrNotFound) {
			log.Fatal().Err(err).Msg("failed to load block offset")
			return
		}
	} else {
		seqno = bo.Seqno
	}

	if *ForceBlock > 0 {
		if *ForceBlock > math.MaxUint32 {
			log.Fatal().Err(err).Msg("block should be uint32")
		}
		seqno = uint32(*ForceBlock)
	}

	inv := make(chan any)
	sc := chain.NewScanner(apiClient, payments.PaymentChannelCodeHash, seqno, scanLog)

	if *UseBlockScanner {
		if err = sc.Start(context.Background(), inv); err != nil {
			log.Fatal().Err(err).Msg("failed to start block scanner")
			return
		}
	} else {
		if err = sc.StartSmall(inv); err != nil {
			log.Fatal().Err(err).Msg("failed to start account scanner")
			return
		}
		fdb.SetOnChannelUpdated(sc.OnChannelUpdate)

		chList, err := fdb.GetChannels(context.Background(), nil, db.ChannelStateAny)
		if err != nil {
			log.Fatal().Err(err).Msg("failed to load channels")
			return
		}

		for _, channel := range chList {
			if channel.Status != db.ChannelStateInactive {
				sc.OnChannelUpdate(channel)
			}
		}
	}

	w, err := wallet.FromPrivateKey(apiClient, cfg.PaymentNodePrivateKey, wallet.ConfigHighloadV3{
		MessageTTL: 3*60 + 30,
		MessageBuilder: func(ctx context.Context, subWalletId uint32) (id uint32, createdAt int64, err error) {
			createdAt = time.Now().Unix() - 30 // something older than last master block, to pass through LS external's time validation
			id = uint32(createdAt) % (1 << 23) // TODO: store seqno in db
			return
		},
	})
	if err != nil {
		log.Fatal().Err(err).Msg("failed to init wallet")
		return
	}
	log.Info().Str("addr", w.WalletAddress().String()).Msg("wallet initialized")

	svc := tonpayments.NewService(apiClient, fdb, tr, w, inv, cfg.PaymentNodePrivateKey, cfg.ChannelConfig)
	tr.SetService(svc)
	log.Info().Hex("pubkey", cfg.PaymentNodePrivateKey.Public().(ed25519.PublicKey)).Msg("node initialized")

	if !*DaemonMode {
		go func() {
			for {
				if err := commandReader(svc); err != nil {
					log.Error().Err(err).Msg("command failed")
				}
			}
		}()
	}

	if *API != "" {
		var credentials *api.Credentials
		if *APICredentialsLogin != "" || *APICredentialsPassword != "" {
			if *APICredentialsLogin == "" || *APICredentialsPassword == "" {
				log.Fatal().Msg("both api login and password must be set in the same time")
				return
			}

			credentials = &api.Credentials{
				Login:    *APICredentialsLogin,
				Password: *APICredentialsPassword,
			}
		}

		srv := api.NewServer(*API, *Webhook, cfg.WebhooksSignatureHMACSHA256Key, svc, fdb, credentials)
		if *Webhook != "" {
			svc.SetWebhook(srv)
		}

		go func() {
			if err := srv.Start(); err != nil {
				log.Error().Err(err).Msg("failed to start api server")
			}
		}()

		log.Info().Str("api", *API).Str("webhook", *Webhook).Msg("api initialized")
	}

	svc.Start()
}

func commandReader(svc *tonpayments.Service) error {
	var cmd string
	_, _ = fmt.Scanln(&cmd)

	switch cmd {
	case "list":
		svc.DebugPrintVirtualChannels()
	case "inc":
		log.Info().Msg("input channel address to run increment state test:")
		var addr string
		_, _ = fmt.Scanln(&addr)

		for i := 0; i < 30; i++ {
			if err := svc.IncrementStates(context.Background(), addr, true); err != nil {
				return fmt.Errorf("failed to increment states with channel: %w", err)
			}
		}
		log.Info().Msg("tasks created")
	case "inc-hard":
		log.Info().Msg("input channel address to run increment state test:")
		var addr string
		_, _ = fmt.Scanln(&addr)

		for i := 0; i < 3000; i++ {
			if err := svc.IncrementStates(context.Background(), addr, true); err != nil {
				return fmt.Errorf("failed to increment states with channel: %w", err)
			}
		}
		log.Info().Msg("tasks created")
	case "destroy":
		log.Info().Msg("to start cooperative close input channel address:")
		var addr string
		_, _ = fmt.Scanln(&addr)

		if err := svc.RequestCooperativeClose(context.Background(), addr); err != nil {
			return fmt.Errorf("failed to close channel cooperatively: %w", err)
		}
		log.Info().Msg("cooperative channel closure attempt has been started")
	case "kill":
		log.Info().Msg("to start uncooperative close input channel address:")
		var addr string
		_, _ = fmt.Scanln(&addr)

		if err := svc.RequestUncooperativeClose(context.Background(), addr); err != nil {
			return fmt.Errorf("failed to close channel uncooperatively: %w", err)
		}
		log.Info().Msg("uncooperative channel closure has been started")
	case "sign":
		log.Info().Msg("input virtual channel private key:")
		var strKey string
		_, _ = fmt.Scanln(&strKey)

		btsKey, err := hex.DecodeString(strKey)
		if err != nil {
			return fmt.Errorf("incorrect format of key: %w", err)
		}
		if len(btsKey) != 32 {
			return fmt.Errorf("incorrect len of key: %d, should be 32", len(btsKey))
		}

		log.Info().Msg("input amount:")
		var strAmt string
		_, _ = fmt.Scanln(&strAmt)

		amt, err := tlb.FromTON(strAmt)
		if err != nil {
			return fmt.Errorf("incorrect format of amount: %w", err)
		}

		vcKey := ed25519.NewKeyFromSeed(btsKey)
		st := &payments.VirtualChannelState{
			Amount: amt,
		}
		st.Sign(vcKey)

		cll, err := st.ToCell()
		if err != nil {
			return fmt.Errorf("failed to serialize cell: %w", err)
		}

		log.Info().Str("signed_state", hex.EncodeToString(vcKey.Public().(ed25519.PublicKey))+hex.EncodeToString(cll.ToBOC())).Msg("state was signed")
	case "close":
		log.Info().Msg("enter the virtual channel final state hex:")

		var state string
		_, _ = fmt.Scanln(&state)

		btsState, err := hex.DecodeString(state)
		if err != nil {
			return fmt.Errorf("incorrect format of state: %w", err)
		}
		if len(btsState) <= 32 {
			return fmt.Errorf("incorrect len of state")
		}

		stateCell, err := cell.FromBOC(btsState[32:])
		if err != nil {
			return fmt.Errorf("incorrect state BoC: %w", err)
		}

		var st payments.VirtualChannelState
		err = tlb.LoadFromCell(&st, stateCell.BeginParse())
		if err != nil {
			return fmt.Errorf("incorrect state cell: %w", err)
		}

		err = svc.AddVirtualChannelResolve(context.Background(), btsState[:32], st)
		if err != nil {
			return fmt.Errorf("failed to add resolve to channel: %w", err)
		}

		err = svc.CloseVirtualChannel(context.Background(), btsState[:32])
		if err != nil {
			return fmt.Errorf("failed to close channel: %w", err)
		}
		log.Info().Msg("virtual channel closure requested")
	case "topup":
		log.Info().Msg("enter channel address to topup:")

		var addrStr string
		_, _ = fmt.Scanln(&addrStr)

		addr, err := address.ParseAddr(addrStr)
		if err != nil {
			return fmt.Errorf("incorrect format of address: %w", err)
		}

		log.Info().Msg("input amount:")
		var strAmt string
		_, _ = fmt.Scanln(&strAmt)

		amt, err := tlb.FromTON(strAmt)
		if err != nil {
			return fmt.Errorf("incorrect format of amount: %w", err)
		}

		if err = svc.TopupChannel(context.Background(), addr, amt); err != nil {
			return fmt.Errorf("failed to topup channel: %w", err)
		}
		log.Info().Str("address", addr.String()).Msg("topup task registered")
	case "withdraw":
		log.Info().Msg("enter channel address to withdraw from:")

		var addrStr string
		_, _ = fmt.Scanln(&addrStr)

		addr, err := address.ParseAddr(addrStr)
		if err != nil {
			return fmt.Errorf("incorrect format of address: %w", err)
		}

		log.Info().Msg("input amount:")
		var strAmt string
		_, _ = fmt.Scanln(&strAmt)

		amt, err := tlb.FromTON(strAmt)
		if err != nil {
			return fmt.Errorf("incorrect format of amount: %w", err)
		}

		if err = svc.RequestWithdraw(context.Background(), addr, amt); err != nil {
			return fmt.Errorf("failed to withdraw from channel: %w", err)
		}
	case "deploy":
		log.Info().Msg("enter the key of node to deploy channel with:")

		var strKey string
		_, _ = fmt.Scanln(&strKey)

		btsKey, err := hex.DecodeString(strKey)
		if err != nil {
			return fmt.Errorf("incorrect format of key: %w", err)
		}
		if len(btsKey) != 32 {
			return fmt.Errorf("incorrect len of key: %d, should be 32", len(btsKey))
		}

		log.Info().Msg("input jetton master address or extra currency id, or skip for ton:")
		var jetton string
		_, _ = fmt.Scanln(&jetton)

		var ecID uint64
		var jettonMaster *address.Address
		if jetton != "" {
			ecID, err = strconv.ParseUint(jetton, 10, 32)
			if err != nil {
				jettonMaster, err = address.ParseAddr(jetton)
				if err != nil {
					return fmt.Errorf("incorrect format: %w", err)
				}
			}
		}

		ctx, cancel := context.WithTimeout(context.Background(), 120*time.Second)
		addr, err := svc.DeployChannelWithNode(ctx, btsKey, jettonMaster, uint32(ecID))
		cancel()
		if err != nil {
			return fmt.Errorf("failed to deploy channel with node: %w", err)
		}
		log.Info().Str("address", addr.String()).Msg("onchain channel deployed")
	case "open", "send":
		log.Info().Msg("enter nodes to tunnel virtual channel through, including receiver (',' separated):")
		var strKeys string
		_, _ = fmt.Scanln(&strKeys)

		keys := strings.Split(strings.ReplaceAll(strKeys, " ", ""), ",")

		var with []byte
		var parsedKeys [][]byte
		for _, strKey := range keys {
			btsKey, err := hex.DecodeString(strKey)
			if err != nil {
				return fmt.Errorf("incorrect format of key: %w", err)
			}
			if len(btsKey) != 32 {
				return fmt.Errorf("incorrect len of key: %d, should be 32", len(btsKey))
			}

			if with == nil {
				with = btsKey
			}

			parsedKeys = append(parsedKeys, btsKey)
		}

		log.Info().Msg("input amount, excluding tunnelling fee:")

		var strAmt string
		_, _ = fmt.Scanln(&strAmt)

		amt, err := tlb.FromTON(strAmt)
		if err != nil {
			return fmt.Errorf("incorrect format of amount: %w", err)
		}

		log.Info().Msg("input fee amount per each proxy node:")

		var strAmtFee string
		_, _ = fmt.Scanln(&strAmtFee)

		amtFee, err := tlb.FromTON(strAmtFee)
		if err != nil {
			return fmt.Errorf("incorrect format of fee amount: %w", err)
		}

		fullAmt := new(big.Int).Set(amt.Nano())
		var tunChain []transport.TunnelChainPart
		for i, parsedKey := range parsedKeys {
			fee := big.NewInt(0)
			if len(parsedKeys)-i > 1 {
				fee = new(big.Int).Mul(amtFee.Nano(), big.NewInt(int64(len(parsedKeys)-i)-1))
				fullAmt = fullAmt.Add(fullAmt, fee)
			}

			tunChain = append(tunChain, transport.TunnelChainPart{
				Target:   parsedKey,
				Capacity: amt.Nano(),
				Fee:      fee,
				Deadline: time.Now().Add(1*time.Hour + (30*time.Minute)*time.Duration(len(parsedKeys)-i)),
			})
		}

		_, vPriv, _ := ed25519.GenerateKey(nil)
		vc, firstInstructionKey, tun, err := transport.GenerateTunnel(vPriv, tunChain, 5, cmd == "send")
		if err != nil {
			return fmt.Errorf("failed to generate tunnel: %w", err)
		}

		ctx, cancel := context.WithTimeout(context.Background(), 8*time.Second)
		err = svc.OpenVirtualChannel(ctx, with, firstInstructionKey, vPriv, tun, vc)
		cancel()
		if err != nil {
			return fmt.Errorf("failed to open virtual channel with node: %w", err)
		}

		if cmd != "send" {
			log.Info().
				Str("private_key", hex.EncodeToString(vPriv.Seed())).
				Str("total_amount", tlb.FromNanoTON(fullAmt).String()).
				Str("capacity", amt.String()).
				Msg("virtual channel opening requested")
		} else {
			log.Info().
				Str("total_amount", tlb.FromNanoTON(fullAmt).String()).
				Str("amount", amt.String()).
				Msg("virtual transfer requested")
		}
	default:
		return fmt.Errorf("unknown command: %s", cmd)
	}

	return nil
}
