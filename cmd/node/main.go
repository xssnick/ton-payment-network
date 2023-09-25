package main

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"flag"
	"fmt"
	"github.com/rs/zerolog"
	"github.com/rs/zerolog/log"
	"github.com/xssnick/payment-network/internal/node"
	"github.com/xssnick/payment-network/internal/node/chain"
	"github.com/xssnick/payment-network/internal/node/db"
	"github.com/xssnick/payment-network/internal/node/db/filedb"
	"github.com/xssnick/payment-network/internal/node/transport"
	"github.com/xssnick/payment-network/pkg/payments"
	"github.com/xssnick/tonutils-go/address"
	"github.com/xssnick/tonutils-go/adnl"
	"github.com/xssnick/tonutils-go/adnl/dht"
	"github.com/xssnick/tonutils-go/liteclient"
	"github.com/xssnick/tonutils-go/tlb"
	"github.com/xssnick/tonutils-go/ton"
	"github.com/xssnick/tonutils-go/ton/wallet"
	"github.com/xssnick/tonutils-go/tvm/cell"
	"golang.org/x/crypto/ed25519"
	"net"
	"time"
)

var Debug = flag.Bool("debug", false, "debug logs")
var IP = flag.String("ip", "", "ip to listen on and store in DHT")
var Port = flag.Uint64("port", 9761, "port to listen on and store in DHT")
var Name = flag.String("name", "", "any string, seed for channel private key")

func main() {
	flag.Parse()

	log.Logger = zerolog.New(zerolog.NewConsoleWriter()).With().Timestamp().Logger().Level(zerolog.InfoLevel)
	if *Debug {
		log.Logger = log.Logger.Level(zerolog.DebugLevel).With().Logger()
	}
	adnl.Logger = func(v ...any) {}

	if *Name == "" {
		log.Fatal().Msg("-name flag should be set")
		return
	}

	log.Info().Msg("initializing ton client with verified proof chain...")

	client := liteclient.NewConnectionPool()

	tonCfg, err := liteclient.GetConfigFromUrl(context.Background(), "https://ton-blockchain.github.io/testnet-global.config.json")
	if err != nil {
		log.Fatal().Err(err).Msg("failed to get config")
		return
	}

	// connect to lite servers
	err = client.AddConnectionsFromConfig(context.Background(), tonCfg)
	if err != nil {
		log.Fatal().Err(err).Msg("ton connect err")
		return
	}

	// TODO: set secure policy
	// initialize ton api lite connection wrapper
	api := ton.NewAPIClient(client, ton.ProofCheckPolicyFast).WithRetry()
	// api.SetTrustedBlockFromConfig(tonCfg)

	sk := sha256.Sum256([]byte("adnl" + *Name))

	serverKey := ed25519.NewKeyFromSeed(sk[:])
	dhtGate := adnl.NewGateway(serverKey)
	if err = dhtGate.StartClient(); err != nil {
		log.Fatal().Err(err).Msg("failed to init adnl gateway for dht")
		return
	}

	dhtClient, err := dht.NewClientFromConfig(dhtGate, tonCfg)
	if err != nil {
		log.Fatal().Err(err).Msg("failed to init dht client")
		return
	}

	gate := adnl.NewGateway(serverKey)

	prepare(api, *Name, gate, dhtClient, serverKey)
}

func prepare(api ton.APIClientWrapped, name string, gate *adnl.Gateway, dhtClient *dht.Client, key ed25519.PrivateKey) {
	hash := sha256.Sum256([]byte(name))
	channelKey := ed25519.NewKeyFromSeed(hash[:])

	println("YOUR PUB KEY:", hex.EncodeToString(channelKey.Public().(ed25519.PublicKey)))

	isServer := false
	if *IP != "" {
		isServer = true
		ip := net.ParseIP(*IP)
		if ip == nil {
			log.Fatal().Msg("incorrect ip format")
			return
		}

		gate.SetExternalIP(ip.To4())
		if err := gate.StartServer(fmt.Sprintf(":%d", *Port)); err != nil {
			log.Fatal().Err(err).Msg("failed to init adnl gateway")
			return
		}
	} else {
		if err := gate.StartClient(); err != nil {
			log.Fatal().Err(err).Msg("failed to init adnl gateway")
			return
		}
	}

	fdb := filedb.NewFileDB("./db/" + name)
	tr := transport.NewServer(dhtClient, gate, key, channelKey, isServer)

	var seqno uint32
	if bo, err := fdb.GetBlockOffset(); err != nil {
		if !errors.Is(err, db.ErrNotFound) {
			log.Fatal().Err(err).Msg("failed to load block offset")
			return
		}
	} else {
		seqno = bo.Seqno
	}

	inv := make(chan any)
	sc := chain.NewScanner(api, payments.AsyncPaymentChannelCodeHash, seqno)
	go sc.Start(context.Background(), inv)

	w, err := wallet.FromPrivateKey(api, channelKey, wallet.HighloadV2Verified)
	if err != nil {
		log.Fatal().Err(err).Msg("failed to init wallet")
		return
	}
	log.Info().Str("addr", w.Address().String()).Msg("wallet initialized")

	svc := node.NewService(api, fdb, tr, w, inv, channelKey, 5*time.Minute, payments.ClosingConfig{
		QuarantineDuration:       60,
		MisbehaviorFine:          tlb.MustFromTON("0.015"),
		ConditionalCloseDuration: 60,
	})
	tr.SetService(svc)

	go func() {
		for {
			var cmd string
			fmt.Scanln(&cmd)

			switch cmd {
			case "list":
				svc.DebugPrintVirtualChannels()
			case "destroy":
				println("Input address:")
				var addr string
				fmt.Scanln(&addr)

				err = svc.RequestCooperativeClose(context.Background(), addr)
				if err != nil {
					println("failed to coop close channel:", err.Error())
					continue
				}
				println("CHANNEL COOP CLOSED")
			case "sign":
				println("Channel private key:")
				var strKey string
				fmt.Scanln(&strKey)

				btsKey, err := hex.DecodeString(strKey)
				if err != nil {
					println("incorrect format of key")
					continue
				}
				if len(btsKey) != 32 {
					println("incorrect len of key")
					continue
				}

				println("Input amount:")
				var strAmt string
				fmt.Scanln(&strAmt)

				amt, err := tlb.FromTON(strAmt)
				if err != nil {
					println("incorrect format of amount")
					continue
				}

				vcKey := ed25519.NewKeyFromSeed(btsKey)
				st := &payments.VirtualChannelState{
					Amount: amt,
				}
				st.Sign(vcKey)

				cll, err := st.ToCell()
				if err != nil {
					println("failed to serialize cell")
					continue
				}

				println("KEY STATE:", hex.EncodeToString(vcKey.Public().(ed25519.PublicKey))+hex.EncodeToString(cll.ToBOC()))
			case "close":
				println("Input key+state:")
				var state string
				fmt.Scanln(&state)

				btsState, err := hex.DecodeString(state)
				if err != nil {
					println("incorrect format of state")
					continue
				}
				if len(btsState) <= 32 {
					println("incorrect len of state")
					continue
				}

				stateCell, err := cell.FromBOC(btsState[32:])
				if err != nil {
					println("incorrect state boc")
					continue
				}

				var st payments.VirtualChannelState
				err = tlb.LoadFromCell(&st, stateCell.BeginParse())
				if err != nil {
					println("incorrect state cell")
					continue
				}

				err = svc.CloseVirtualChannel(context.Background(), btsState[:32], st)
				if err != nil {
					println("failed to close channel:", err.Error())
					continue
				}
				println("VIRTUAL CHANNEL CLOSED")
			case "deploy_out":
				println("With node key:")
				var strKey string
				fmt.Scanln(&strKey)

				btsKey, err := hex.DecodeString(strKey)
				if err != nil {
					println("incorrect format of key")
					continue
				}
				if len(btsKey) != 32 {
					println("incorrect len of key")
					continue
				}

				println("Input amount:")
				var strAmt string
				fmt.Scanln(&strAmt)

				amt, err := tlb.FromTON(strAmt)
				if err != nil {
					println("incorrect format of amount")
					continue
				}

				chId := sha256.Sum256([]byte("default"))
				addr, err := svc.DeployChannelWithNode(context.Background(), chId[:16], btsKey, amt)
				if err != nil {
					log.Error().Err(err).Msg("failed to deploy channel with node")
					continue
				}
				println("DEPLOYED:", addr.String())
			case "deploy_in":
				println("With node key:")
				var strKey string
				fmt.Scanln(&strKey)

				btsKey, err := hex.DecodeString(strKey)
				if err != nil {
					println("incorrect format of key")
					continue
				}
				if len(btsKey) != 32 {
					println("incorrect len of key")
					continue
				}

				println("Input amount:")
				var strAmt string
				fmt.Scanln(&strAmt)

				amt, err := tlb.FromTON(strAmt)
				if err != nil {
					println("incorrect format of amount")
					continue
				}

				err = svc.RequestInboundChannel(context.Background(), amt, btsKey)
				if err != nil {
					log.Error().Err(err).Msg("failed to request deploy channel with node")
					continue
				}
				println("DEPLOY REQUESTED")
			case "open":
				println("Receiver key:")
				var strKey string
				fmt.Scanln(&strKey)

				btsKey, err := hex.DecodeString(strKey)
				if err != nil {
					println("incorrect format of key")
					continue
				}
				if len(btsKey) != 32 {
					println("incorrect len of key")
					continue
				}

				println("Onchain channel with node addr:")
				var strAddr string
				fmt.Scanln(&strAddr)

				addr, err := address.ParseAddr(strAddr)
				if err != nil {
					println("incorrect format of amount")
					continue
				}

				println("Input amount (exclude 0.02 fee):")
				var strAmt string
				fmt.Scanln(&strAmt)

				amt, err := tlb.FromTON(strAmt)
				if err != nil {
					println("incorrect format of amount")
					continue
				}

				cell.BeginCell().MustStoreAddr(address.MustParseAddr("EQCajaUU1XXSAjTD-xOV7pE49fGtg4q8kF3ELCOJtGvQFQ2C")).EndCell().BeginParse()

				vPub, vPriv, _ := ed25519.GenerateKey(nil)

				ctx, cancel := context.WithTimeout(context.Background(), 8*time.Second)
				err = svc.OpenVirtualChannel(ctx, addr.String(), btsKey, payments.VirtualChannel{
					Key:      vPub,
					Capacity: amt.Nano(),
					Fee:      tlb.MustFromTON("0.02").Nano(),
					Deadline: time.Now().Unix() + 3600,
				})
				cancel()
				if err != nil {
					log.Error().Err(err).Msg("failed to open virtual channel with node")
					continue
				}

				println("VIRTUAL CHANNEL IS OPEN, PRIV KEY:", hex.EncodeToString(vPriv.Seed()))
			default:
				println("UNKNOWN COMMAND " + cmd)
			}
		}
	}()

	svc.Start()
}
