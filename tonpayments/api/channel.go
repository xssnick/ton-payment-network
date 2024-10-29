package api

import (
	"context"
	"crypto/ed25519"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"github.com/xssnick/ton-payment-network/tonpayments/db"
	"github.com/xssnick/tonutils-go/address"
	"github.com/xssnick/tonutils-go/tlb"
	"net/http"
	"time"
)

type Onchain struct {
	CommittedSeqno uint32 `json:"committed_seqno"`
	WalletAddress  string `json:"wallet_address"`
	Deposited      string `json:"deposited"`
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
	AcceptingActions bool   `json:"accepting_actions"`
	Status           string `json:"status"`
	WeLeft           bool   `json:"we_left"`
	Our              Side   `json:"our"`
	Their            Side   `json:"their"`

	InitAt    time.Time `json:"init_at"`
	UpdatedAt time.Time `json:"updated_at"`
	CreatedAt time.Time `json:"created_at"`
}

func (s *Server) handleChannelOpen(w http.ResponseWriter, r *http.Request) {
	type request struct {
		WithNode     string `json:"with_node"`
		Capacity     string `json:"capacity"`
		JettonMaster string `json:"jetton_master"`
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

	amt, err := tlb.FromTON(req.Capacity)
	if err != nil {
		writeErr(w, 400, "incorrect amount format: "+err.Error())
		return
	}

	var jetton *address.Address
	if req.JettonMaster != "" {
		jetton, err = address.ParseAddr(req.JettonMaster)
		if err != nil {
			writeErr(w, 400, "incorrect jetton address format: "+err.Error())
			return
		}
	}

	addr, err := s.svc.DeployChannelWithNode(r.Context(), amt, key, jetton)
	if err != nil {
		writeErr(w, 500, "failed to deploy channel: "+err.Error())
		return
	}

	writeResp(w, response{
		Address: addr.String(),
	})
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
		v, err := convertChannel(channel)
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

	res, err := convertChannel(ch)
	if err != nil {
		writeErr(w, 500, "failed to convert channel: "+err.Error())
		return
	}

	writeResp(w, res)
}

func convertChannel(c *db.Channel) (OnchainChannel, error) {
	status := "inactive"
	switch c.Status {
	case db.ChannelStateActive:
		status = "active"
	case db.ChannelStateClosing:
		status = "closing"
	}

	theirBalance, err := c.CalcBalance(true)
	if err != nil {
		return OnchainChannel{}, fmt.Errorf("failed to calc balance: %w", err)
	}
	ourBalance, err := c.CalcBalance(true)
	if err != nil {
		return OnchainChannel{}, fmt.Errorf("failed to calc balance: %w", err)
	}

	return OnchainChannel{
		ID:               hex.EncodeToString(c.ID),
		Address:          c.Address,
		JettonAddress:    c.JettonAddress,
		AcceptingActions: c.AcceptingActions,
		Status:           status,
		WeLeft:           c.WeLeft,
		Our: Side{
			Key:              hex.EncodeToString(c.OurOnchain.Key),
			AvailableBalance: tlb.FromNanoTON(ourBalance).String(),
			Onchain: Onchain{
				CommittedSeqno: c.OurOnchain.CommittedSeqno,
				WalletAddress:  c.OurOnchain.WalletAddress,
				Deposited:      tlb.FromNanoTON(c.OurOnchain.Deposited).String(),
			},
		},
		Their: Side{
			Key:              hex.EncodeToString(c.TheirOnchain.Key),
			AvailableBalance: tlb.FromNanoTON(theirBalance).String(),
			Onchain: Onchain{
				CommittedSeqno: c.TheirOnchain.CommittedSeqno,
				WalletAddress:  c.TheirOnchain.WalletAddress,
				Deposited:      tlb.FromNanoTON(c.TheirOnchain.Deposited).String(),
			},
		},
		InitAt:    c.InitAt,
		UpdatedAt: c.UpdatedAt,
		CreatedAt: c.CreatedAt,
	}, nil
}

func (s *Server) PushChannelEvent(ctx context.Context, ch *db.Channel) error {
	res, err := convertChannel(ch)
	if err != nil {
		return fmt.Errorf("failed to convert channel: %w", err)
	}

	if err = s.queue.CreateTask(ctx, WebhooksTaskPool, "onchain-channel-event", "events",
		ch.Address+"-"+fmt.Sprint(res.UpdatedAt.UnixNano()),
		res, nil, nil,
	); err != nil {
		return fmt.Errorf("failed to create ask-remove-virtual task: %w", err)
	}

	return nil
}
