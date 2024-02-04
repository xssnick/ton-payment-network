package api

import (
	"context"
	"crypto/ed25519"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"github.com/xssnick/ton-payment-network/pkg/payments"
	"github.com/xssnick/ton-payment-network/tonpayments/db"
	"github.com/xssnick/ton-payment-network/tonpayments/transport"
	"github.com/xssnick/tonutils-go/address"
	"github.com/xssnick/tonutils-go/tlb"
	"github.com/xssnick/tonutils-go/tvm/cell"
	"net/http"
	"time"
)

type NodeChain struct {
	Key                string `json:"key"`
	Fee                string `json:"fee"`
	DeadlineGapSeconds int64  `json:"deadline_gap_seconds"`
}

type VirtualSide struct {
	ChannelAddress string    `json:"channel_address"`
	Capacity       string    `json:"capacity"`
	Fee            string    `json:"fee"`
	DeadlineAt     time.Time `json:"deadline_at"`
}

type VirtualChannel struct {
	Key             string       `json:"key"`
	Active          bool         `json:"active"`
	LastKnownAmount string       `json:"last_known_amount"`
	CreatedAt       time.Time    `json:"created_at"`
	Outgoing        *VirtualSide `json:"outgoing"`
	Incoming        *VirtualSide `json:"incoming"`
}

func (s *Server) handleVirtualGet(w http.ResponseWriter, r *http.Request) {
	if r.Method != "GET" {
		writeErr(w, 400, "incorrect request method")
		return
	}

	var err error
	var key ed25519.PublicKey
	if q := r.URL.Query().Get("key"); q != "" {
		key, err = parseKey(q)
		if err != nil {
			writeErr(w, 400, "incorrect key format: "+err.Error())
			return
		}
	} else {
		writeErr(w, 400, "channel address is not passed")
	}

	res, err := s.getVirtual(r.Context(), key)
	if err != nil {
		writeErr(w, 500, err.Error())
		return
	}

	writeResp(w, res)
}

func (s *Server) handleVirtualList(w http.ResponseWriter, r *http.Request) {
	if r.Method != "GET" {
		writeErr(w, 400, "incorrect request method")
		return
	}

	var err error
	var addr *address.Address
	if q := r.URL.Query().Get("addr"); q != "" {
		addr, err = address.ParseAddr(q)
		if err != nil {
			writeErr(w, 400, "incorrect address format: "+err.Error())
			return
		}
	} else {
		writeErr(w, 400, "channel address is not passed")
	}

	ch, err := s.svc.GetChannel(r.Context(), addr.String())
	if err != nil {
		writeErr(w, 500, "failed to get channel: "+err.Error())
		return
	}

	var our, their []*VirtualChannel

	// TODO: can be too heavy, optimize
	for _, kv := range ch.Their.State.Data.Conditionals.All() {
		vch, err := payments.ParseVirtualChannelCond(kv.Value)
		if err != nil {
			continue
		}

		res, err := s.getVirtual(r.Context(), vch.Key)
		if err != nil {
			writeErr(w, 500, err.Error())
			return
		}
		their = append(their, res)
	}

	for _, kv := range ch.Our.State.Data.Conditionals.All() {
		vch, err := payments.ParseVirtualChannelCond(kv.Value)
		if err != nil {
			continue
		}

		res, err := s.getVirtual(r.Context(), vch.Key)
		if err != nil {
			writeErr(w, 500, err.Error())
			return
		}
		our = append(our, res)
	}

	writeResp(w, struct {
		Their []*VirtualChannel `json:"their"`
		Our   []*VirtualChannel `json:"our"`
	}{their, our})
}

func (s *Server) getVirtual(ctx context.Context, key ed25519.PublicKey) (*VirtualChannel, error) {
	meta, err := s.svc.GetVirtualChannelMeta(ctx, key)
	if err != nil {
		return nil, fmt.Errorf("failed to get virtual channel: %w", err)
	}

	res := &VirtualChannel{
		Key:             hex.EncodeToString(meta.Key),
		Active:          meta.Active,
		CreatedAt:       meta.CreatedAt,
		LastKnownAmount: "0",
	}

	if len(meta.LastKnownResolve) > 0 {
		cll, err := cell.FromBOC(meta.LastKnownResolve)
		if err != nil {
			return nil, fmt.Errorf("failed to parse last known resolve BoC: %w", err)
		}

		var st payments.VirtualChannelState
		if err = tlb.LoadFromCell(&st, cll.BeginParse()); err != nil {
			return nil, fmt.Errorf("failed to parse last known resolve state: %w", err)
		}

		if !st.Verify(meta.Key) {
			return nil, fmt.Errorf("failed to verify last known resolve state: %w", err)
		}

		res.LastKnownAmount = st.Amount.String()
	}

	if res.Active {
		if meta.FromChannelAddress != "" {
			from, err := s.svc.GetChannel(ctx, meta.FromChannelAddress)
			if err != nil {
				return nil, fmt.Errorf("failed to get 'from' channel: %w", err)
			}

			_, vch, err := from.Their.State.FindVirtualChannel(meta.Key)
			if err != nil {
				return nil, fmt.Errorf("failed to find virtual 'from' channel: %w", err)
			}

			res.Incoming = &VirtualSide{
				ChannelAddress: meta.FromChannelAddress,
				Capacity:       tlb.FromNanoTON(vch.Capacity).String(),
				Fee:            tlb.FromNanoTON(vch.Fee).String(),
				DeadlineAt:     time.Unix(vch.Deadline, 0),
			}
		}

		if meta.ToChannelAddress != "" {
			to, err := s.svc.GetChannel(ctx, meta.ToChannelAddress)
			if err != nil {
				return nil, fmt.Errorf("failed to get 'to channel: %w", err)
			}

			_, vch, err := to.Our.State.FindVirtualChannel(meta.Key)
			if err != nil {
				return nil, fmt.Errorf("failed to find virtual 'to' channel: %w", err)
			}

			res.Outgoing = &VirtualSide{
				ChannelAddress: meta.FromChannelAddress,
				Capacity:       tlb.FromNanoTON(vch.Capacity).String(),
				Fee:            tlb.FromNanoTON(vch.Fee).String(),
				DeadlineAt:     time.Unix(vch.Deadline, 0),
			}
		}
	}

	return res, nil
}

func (s *Server) handleVirtualState(w http.ResponseWriter, r *http.Request) {
	type request struct {
		Key   string `json:"key"`
		State []byte `json:"state"`
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

	key, err := parseKey(req.Key)
	if err != nil {
		writeErr(w, 400, "failed to parse key: "+err.Error())
		return
	}

	cll, err := cell.FromBOC(req.State)
	if err != nil {
		writeErr(w, 400, "failed to parse state: "+err.Error())
		return
	}

	var st payments.VirtualChannelState
	if err = tlb.LoadFromCell(&st, cll.BeginParse()); err != nil {
		writeErr(w, 400, "failed to parse last known resolve state: "+err.Error())
		return
	}

	if !st.Verify(key) {
		writeErr(w, 400, "failed to verify last known resolve state: "+err.Error())
		return
	}

	if err = s.svc.AddVirtualChannelResolve(r.Context(), key, st); err != nil && !errors.Is(err, db.ErrNewerStateIsKnown) {
		writeErr(w, 500, "failed to add virtual channel state: "+err.Error())
		return
	}

	writeSuccess(w)
}

func (s *Server) handleVirtualClose(w http.ResponseWriter, r *http.Request) {
	type request struct {
		Key   string `json:"key"`
		State []byte `json:"state"`
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

	key, err := parseKey(req.Key)
	if err != nil {
		writeErr(w, 400, "failed to parse key: "+err.Error())
		return
	}

	cll, err := cell.FromBOC(req.State)
	if err != nil {
		writeErr(w, 400, "failed to parse state: "+err.Error())
		return
	}

	var st payments.VirtualChannelState
	if err = tlb.LoadFromCell(&st, cll.BeginParse()); err != nil {
		writeErr(w, 400, "failed to parse last known resolve state: "+err.Error())
		return
	}

	if !st.Verify(key) {
		writeErr(w, 400, "failed to verify last known resolve state: "+err.Error())
		return
	}

	if err = s.svc.AddVirtualChannelResolve(r.Context(), key, st); err != nil && !errors.Is(err, db.ErrNewerStateIsKnown) {
		writeErr(w, 500, "failed to add virtual channel state: "+err.Error())
		return
	}

	if err = s.svc.CloseVirtualChannel(r.Context(), key); err != nil {
		writeErr(w, 500, "failed to close virtual channel: "+err.Error())
		return
	}

	writeSuccess(w)
}

func (s *Server) handleVirtualOpen(w http.ResponseWriter, r *http.Request) {
	type request struct {
		TTLSeconds int64       `json:"ttl_seconds"`
		Capacity   string      `json:"capacity"`
		NodesChain []NodeChain `json:"nodes_chain"`
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

	if len(req.NodesChain) == 0 {
		writeErr(w, 400, "no nodes passed")
		return
	}

	deadline := time.Now().Add(time.Duration(req.TTLSeconds) * time.Second)

	deadlines := make([]time.Time, len(req.NodesChain))
	for i := range req.NodesChain {
		deadlines[i] = deadline
		deadline = deadline.Add(time.Duration(req.NodesChain[i].DeadlineGapSeconds) * time.Second)
	}

	capacity, err := tlb.FromTON(req.Capacity)
	if err != nil {
		writeErr(w, 400, "failed to parse capacity: "+err.Error())
		return
	}

	var with []byte
	var tunChain []transport.TunnelChainPart
	for i, node := range req.NodesChain {
		key, err := parseKey(node.Key)
		if err != nil {
			writeErr(w, 400, "failed to parse node "+fmt.Sprint(i)+" key: "+err.Error())
			return
		}

		fee, err := tlb.FromTON(node.Fee)
		if err != nil {
			writeErr(w, 400, "failed to parse node "+fmt.Sprint(i)+" fee: "+err.Error())
			return
		}

		if with == nil {
			with = key
		}

		tunChain = append(tunChain, transport.TunnelChainPart{
			Target:   key,
			Capacity: capacity.Nano(),
			Fee:      fee.Nano(),
			Deadline: deadlines[i],
		})
	}

	_, vPriv, err := ed25519.GenerateKey(nil)
	if err != nil {
		writeErr(w, 500, "failed to generate key: "+err.Error())
		return
	}

	vc, firstInstructionKey, tun, err := transport.GenerateTunnel(vPriv, tunChain, 5, false)
	if err != nil {
		writeErr(w, 500, "failed to generate tunnel: "+err.Error())
		return
	}

	err = s.svc.OpenVirtualChannel(r.Context(), with, firstInstructionKey, vPriv, tun, vc)
	if err != nil {
		writeErr(w, 403, "failed to request virtual channel open: "+err.Error())
		return
	}

	writeResp(w, struct {
		PublicKey      string    `json:"public_key"`
		PrivateKeySeed string    `json:"private_key_seed"`
		Status         string    `json:"status"`
		Deadline       time.Time `json:"deadline"`
	}{
		PublicKey:      hex.EncodeToString(vPriv.Public().(ed25519.PublicKey)),
		PrivateKeySeed: hex.EncodeToString(vPriv.Seed()),
		Status:         "pending",
		Deadline:       deadlines[len(req.NodesChain)-1],
	})
}

func (s *Server) handleVirtualTransfer(w http.ResponseWriter, r *http.Request) {
	type request struct {
		TTLSeconds int64       `json:"ttl_seconds"`
		Amount     string      `json:"amount"`
		NodesChain []NodeChain `json:"nodes_chain"`
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

	if len(req.NodesChain) == 0 {
		writeErr(w, 400, "no nodes passed")
		return
	}

	deadline := time.Now().Add(time.Duration(req.TTLSeconds) * time.Second)

	deadlines := make([]time.Time, len(req.NodesChain))
	for i := range req.NodesChain {
		deadlines[i] = deadline
		deadline = deadline.Add(time.Duration(req.NodesChain[i].DeadlineGapSeconds) * time.Second)
	}

	capacity, err := tlb.FromTON(req.Amount)
	if err != nil {
		writeErr(w, 400, "failed to parse capacity: "+err.Error())
		return
	}

	var with []byte
	var tunChain []transport.TunnelChainPart
	for i, node := range req.NodesChain {
		key, err := parseKey(node.Key)
		if err != nil {
			writeErr(w, 400, "failed to parse node "+fmt.Sprint(i)+" key: "+err.Error())
			return
		}

		fee, err := tlb.FromTON(node.Fee)
		if err != nil {
			writeErr(w, 400, "failed to parse node "+fmt.Sprint(i)+" fee: "+err.Error())
			return
		}

		if with == nil {
			with = key
		}

		tunChain = append(tunChain, transport.TunnelChainPart{
			Target:   key,
			Capacity: capacity.Nano(),
			Fee:      fee.Nano(),
			Deadline: deadlines[i],
		})
	}

	_, vPriv, err := ed25519.GenerateKey(nil)
	if err != nil {
		writeErr(w, 500, "failed to generate key: "+err.Error())
		return
	}

	vc, firstInstructionKey, tun, err := transport.GenerateTunnel(vPriv, tunChain, 5, true)
	if err != nil {
		writeErr(w, 500, "failed to generate tunnel: "+err.Error())
		return
	}

	err = s.svc.OpenVirtualChannel(r.Context(), with, firstInstructionKey, vPriv, tun, vc)
	if err != nil {
		writeErr(w, 403, "failed to request virtual channel open: "+err.Error())
		return
	}

	writeResp(w, struct {
		Status   string    `json:"status"`
		Deadline time.Time `json:"deadline"`
	}{
		Status:   "pending",
		Deadline: deadlines[len(req.NodesChain)-1],
	})
}
