package api

import (
	"context"
	"crypto/ed25519"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"github.com/xssnick/ton-payment-network/tonpayments/config"
	"github.com/xssnick/ton-payment-network/tonpayments/db"
	"github.com/xssnick/tonutils-go/address"
	"github.com/xssnick/tonutils-go/tlb"
	"math/big"
	"net/http"
	"time"
)

type Onchain struct {
	CommittedSeqno uint64 `json:"committed_seqno"`
	WalletAddress  string `json:"wallet_address"`
	Deposited      string `json:"deposited"`
	Withdrawn      string `json:"withdrawn"`
}

type Side struct {
	Key              string  `json:"key"`
	AvailableBalance string  `json:"available_balance"`
	Onchain          Onchain `json:"onchain"`
}

type OnchainChannel struct {
	ID               string `json:"id"`
	Address          string `json:"address"`
	JettonAddress    string `json:"jetton_address"`
	ExtraCurrencyID  uint32 `json:"ec_id"`
	AcceptingActions bool   `json:"accepting_actions"`
	Status           string `json:"status"`
	WeLeft           bool   `json:"we_left"`
	Our              Side   `json:"our"`
	Their            Side   `json:"their"`

	InitAt          time.Time `json:"init_at"`
	CreatedAt       time.Time `json:"created_at"`
	LastProcessedLT uint64    `json:"processed_lt"`
}

func (s *Server) handleChannelOpen(w http.ResponseWriter, r *http.Request) {
	type request struct {
		WithNode        string `json:"with_node"`
		JettonMaster    string `json:"jetton_master"`
		ExtraCurrencyID uint32 `json:"ec_id"`
	}
	type response struct {
		Address string `json:"address"`
	}

	if r.Method != "POST" {
		writeErr(w, 400, "incorrect request method")
		return
	}

	var req request
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeErr(w, 400, "incorrect request body: "+err.Error())
		return
	}

	key, err := parseKey(req.WithNode)
	if err != nil {
		writeErr(w, 400, "incorrect node key format: "+err.Error())
		return
	}

	var jetton *address.Address
	if req.JettonMaster != "" {
		jetton, err = address.ParseAddr(req.JettonMaster)
		if err != nil {
			writeErr(w, 400, "incorrect jetton address format: "+err.Error())
			return
		}

		if req.ExtraCurrencyID != 0 {
			writeErr(w, 400, "jetton master address and extra currency id are mutually exclusive")
			return
		}
	}

	addr, err := s.svc.OpenChannelWithNode(r.Context(), key, jetton, req.ExtraCurrencyID)
	if err != nil {
		writeErr(w, 500, "failed to open channel: "+err.Error())
		return
	}

	writeResp(w, response{
		Address: addr.String(),
	})
}

func (s *Server) handleTopup(w http.ResponseWriter, r *http.Request) {
	type request struct {
		Address string `json:"address"`
		Amount  string `json:"amount_nano"`
	}

	if r.Method != "POST" {
		writeErr(w, 400, "incorrect request method")
		return
	}

	var req request
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeErr(w, 400, "incorrect request body: "+err.Error())
		return
	}

	amt, _ := new(big.Int).SetString(req.Amount, 10)
	if amt == nil || amt.Sign() <= 0 || amt.BitLen() > 256 {
		writeErr(w, 400, "incorrect amount format")
		return
	}

	addr, err := address.ParseAddr(req.Address)
	if err != nil {
		writeErr(w, 400, "incorrect channel address format: "+err.Error())
		return
	}

	ch, err := s.svc.GetActiveChannel(r.Context(), addr.String())
	if err != nil {
		writeErr(w, 500, "failed to get channel: "+err.Error())
		return
	}

	cc, err := s.svc.ResolveCoinConfig(ch.JettonAddress, ch.ExtraCurrencyID, true)
	if err != nil {
		writeErr(w, 500, "failed to resolve coin config: "+err.Error())
		return
	}

	if err = s.svc.TopupChannel(r.Context(), ch, cc.MustAmount(amt)); err != nil {
		writeErr(w, 500, "failed to topup channel: "+err.Error())
		return
	}

	writeSuccess(w)
}

func (s *Server) handleWithdraw(w http.ResponseWriter, r *http.Request) {
	type request struct {
		Address            string `json:"address"`
		Amount             string `json:"amount_nano"`
		ExecuteOnOtherSide bool   `json:"execute_on_other_side"`
	}

	if r.Method != "POST" {
		writeErr(w, 400, "incorrect request method")
		return
	}

	var req request
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeErr(w, 400, "incorrect request body: "+err.Error())
		return
	}

	amt, _ := new(big.Int).SetString(req.Amount, 10)
	if amt == nil || amt.Sign() <= 0 || amt.BitLen() > 256 {
		writeErr(w, 400, "incorrect amount format")
		return
	}

	addr, err := address.ParseAddr(req.Address)
	if err != nil {
		writeErr(w, 400, "incorrect channel address format: "+err.Error())
		return
	}

	ch, err := s.svc.GetChannel(r.Context(), addr.String())
	if err != nil {
		writeErr(w, 500, "failed to get channel: "+err.Error())
		return
	}

	cc, err := s.svc.ResolveCoinConfig(ch.JettonAddress, ch.ExtraCurrencyID, false)
	if err != nil {
		writeErr(w, 500, "failed to resolve coin config: "+err.Error())
		return
	}

	amtCoin, err := tlb.FromNano(amt, int(cc.Decimals))
	if err != nil {
		writeErr(w, 400, "failed to convert amount: "+err.Error())
	}

	if err = s.svc.RequestWithdraw(r.Context(), addr, amtCoin, !req.ExecuteOnOtherSide); err != nil {
		writeErr(w, 500, "failed to request withdraw channel: "+err.Error())
		return
	}

	writeSuccess(w)
}

func (s *Server) handleChannelClose(w http.ResponseWriter, r *http.Request) {
	type request struct {
		Address string `json:"address"`
		Force   bool   `json:"force"`
	}

	if r.Method != "POST" {
		writeErr(w, 400, "incorrect request method")
		return
	}

	var req request
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeErr(w, 400, "incorrect request body: "+err.Error())
		return
	}

	addr, err := address.ParseAddr(req.Address)
	if err != nil {
		writeErr(w, 400, "incorrect amount format: "+err.Error())
		return
	}

	if req.Force {
		if err = s.svc.RequestUncooperativeClose(r.Context(), addr.String()); err != nil {
			writeErr(w, 500, "failed to uncooperative close channel: "+err.Error())
			return
		}
	} else {
		if err = s.svc.RequestCooperativeClose(r.Context(), addr.String()); err != nil {
			writeErr(w, 500, "failed to cooperative close channel: "+err.Error())
			return
		}
	}

	writeSuccess(w)
}

func (s *Server) handleChannelsList(w http.ResponseWriter, r *http.Request) {
	if r.Method != "GET" {
		writeErr(w, 400, "incorrect request method")
		return
	}

	var err error
	var key ed25519.PublicKey
	var status = db.ChannelStateAny

	if qKey := r.URL.Query().Get("key"); qKey != "" {
		key, err = parseKey(qKey)
		if err != nil {
			writeErr(w, 400, "incorrect node key format: "+err.Error())
			return
		}
	}

	if q := r.URL.Query().Get("status"); q != "" {
		switch q {
		case "active":
			status = db.ChannelStateActive
		case "closing":
			status = db.ChannelStateClosing
		case "inactive":
			status = db.ChannelStateInactive
		case "any":
		default:
			writeErr(w, 400, "unknown status: "+q)
			return
		}
	}

	list, err := s.svc.ListChannels(r.Context(), key, status)
	if err != nil {
		writeErr(w, 500, "failed to list channels: "+err.Error())
		return
	}

	res := make([]OnchainChannel, 0, len(list))
	for i, channel := range list {
		cc, err := s.svc.ResolveCoinConfig(channel.JettonAddress, channel.ExtraCurrencyID, false)
		if err != nil {
			writeErr(w, 500, "failed to resolve coin config: "+err.Error())
			return
		}

		v, err := convertChannel(channel, cc)
		if err != nil {
			writeErr(w, 500, "failed to convert channel "+fmt.Sprint(i)+": "+err.Error())
			return
		}
		res = append(res, v)
	}

	writeResp(w, res)
}

func (s *Server) handleChannelGet(w http.ResponseWriter, r *http.Request) {
	if r.Method != "GET" {
		writeErr(w, 400, "incorrect request method")
		return
	}

	var err error
	var addr *address.Address
	if q := r.URL.Query().Get("address"); q != "" {
		addr, err = address.ParseAddr(q)
		if err != nil {
			writeErr(w, 400, "incorrect address format: "+err.Error())
			return
		}
	} else {
		writeErr(w, 400, "channel address is not passed")
		return
	}

	ch, err := s.svc.GetChannel(r.Context(), addr.String())
	if err != nil {
		writeErr(w, 500, "failed to get channel: "+err.Error())
		return
	}

	cc, err := s.svc.ResolveCoinConfig(ch.JettonAddress, ch.ExtraCurrencyID, false)
	if err != nil {
		writeErr(w, 500, "failed to resolve coin config: "+err.Error())
		return
	}

	res, err := convertChannel(ch, cc)
	if err != nil {
		writeErr(w, 500, "failed to convert channel: "+err.Error())
		return
	}

	writeResp(w, res)
}

func convertChannel(c *db.Channel, cc *config.CoinConfig) (OnchainChannel, error) {
	var status string
	switch c.Status {
	case db.ChannelStateActive:
		status = "active"
	case db.ChannelStateClosing:
		status = "closing"
	default:
		status = "inactive"
	}

	theirBalance, _, err := c.CalcBalance(true)
	if err != nil {
		return OnchainChannel{}, fmt.Errorf("failed to calc balance: %w", err)
	}
	ourBalance, _, err := c.CalcBalance(true)
	if err != nil {
		return OnchainChannel{}, fmt.Errorf("failed to calc balance: %w", err)
	}

	return OnchainChannel{
		ID:               base64.StdEncoding.EncodeToString(c.ID),
		Address:          c.Address,
		JettonAddress:    c.JettonAddress,
		ExtraCurrencyID:  c.ExtraCurrencyID,
		AcceptingActions: c.AcceptingActions,
		Status:           status,
		WeLeft:           c.WeLeft,
		Our: Side{
			Key:              base64.StdEncoding.EncodeToString(c.OurOnchain.Key),
			AvailableBalance: tlb.MustFromNano(ourBalance, int(cc.Decimals)).String(),
			Onchain: Onchain{
				CommittedSeqno: c.OurOnchain.CommittedSeqno,
				WalletAddress:  c.OurOnchain.WalletAddress,
				Deposited:      tlb.MustFromNano(c.OurOnchain.Deposited, int(cc.Decimals)).String(),
			},
		},
		Their: Side{
			Key:              base64.StdEncoding.EncodeToString(c.TheirOnchain.Key),
			AvailableBalance: tlb.MustFromNano(theirBalance, int(cc.Decimals)).String(),
			Onchain: Onchain{
				CommittedSeqno: c.TheirOnchain.CommittedSeqno,
				WalletAddress:  c.TheirOnchain.WalletAddress,
				Deposited:      tlb.MustFromNano(c.TheirOnchain.Deposited, int(cc.Decimals)).String(),
			},
		},
		InitAt:          c.InitAt,
		LastProcessedLT: c.LastProcessedLT,
		CreatedAt:       c.CreatedAt,
	}, nil
}

func (s *Server) PushChannelEvent(ctx context.Context, ch *db.Channel) error {
	cc, err := s.svc.ResolveCoinConfig(ch.JettonAddress, ch.ExtraCurrencyID, false)
	if err != nil {
		return fmt.Errorf("failed to resolve coin config: %w", err)
	}

	res, err := convertChannel(ch, cc)
	if err != nil {
		return fmt.Errorf("failed to convert channel: %w", err)
	}

	if err = s.queue.CreateTask(ctx, WebhooksTaskPool, "onchain-channel-event", "events",
		ch.Address+"-"+fmt.Sprint(res.LastProcessedLT),
		res, nil, nil,
	); err != nil {
		return fmt.Errorf("failed to create webhook task: %w", err)
	}

	return nil
}
