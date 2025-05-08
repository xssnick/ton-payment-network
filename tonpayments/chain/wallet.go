package chain

import (
	"context"
	"crypto/ed25519"
	"github.com/xssnick/tonutils-go/ton/wallet"
	"sync/atomic"
	"time"
)

func InitWallet(apiClient wallet.TonAPI, key ed25519.PrivateKey) (*wallet.Wallet, error) {
	walletAbstractSeqno := uint32(0)
	w, err := wallet.FromPrivateKey(apiClient, key, wallet.ConfigHighloadV3{
		MessageTTL: 3*60 + 30,
		MessageBuilder: func(ctx context.Context, subWalletId uint32) (id uint32, createdAt int64, err error) {
			createdAt = time.Now().UTC().Unix() - 30 // something older than last master block, to pass through LS external's time validation
			// TODO: store seqno in db
			id = uint32((createdAt%(3*60+30))<<15) | atomic.AddUint32(&walletAbstractSeqno, 1)%(1<<15)
			return
		},
	})
	if err != nil {
		return nil, err
	}
	return w, nil
}
