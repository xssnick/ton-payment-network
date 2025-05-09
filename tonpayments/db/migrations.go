package db

import (
	"context"
	"crypto/ed25519"
	"fmt"
	"github.com/rs/zerolog/log"
	"time"
)

type DB interface {
	Transaction(ctx context.Context, f func(ctx context.Context) error) error
	CreateTask(ctx context.Context, poolName, typ, queue, id string, data any, executeAfter, executeTill *time.Time) error
	AcquireTask(ctx context.Context, poolName string) (*Task, error)
	RetryTask(ctx context.Context, task *Task, reason string, retryAt time.Time) error
	CompleteTask(ctx context.Context, poolName string, task *Task) error
	ListActiveTasks(ctx context.Context, poolName string) ([]*Task, error)

	GetVirtualChannelMeta(ctx context.Context, key []byte) (*VirtualChannelMeta, error)
	UpdateVirtualChannelMeta(ctx context.Context, meta *VirtualChannelMeta) error
	CreateVirtualChannelMeta(ctx context.Context, meta *VirtualChannelMeta) error

	SetBlockOffset(ctx context.Context, seqno uint32) error
	GetBlockOffset(ctx context.Context) (*BlockOffset, error)

	GetChannels(ctx context.Context, key ed25519.PublicKey, status ChannelStatus) ([]*Channel, error)
	CreateChannel(ctx context.Context, channel *Channel) error
	GetChannel(ctx context.Context, addr string) (*Channel, error)
	UpdateChannel(ctx context.Context, channel *Channel) error
	SetOnChannelUpdated(f func(ctx context.Context, ch *Channel, statusChanged bool))
	GetOnChannelUpdated() func(ctx context.Context, ch *Channel, statusChanged bool)

	GetUrgentPeers(ctx context.Context) ([][]byte, error)
	AddUrgentPeer(ctx context.Context, peerAddress []byte) error

	Close()

	SetMigrationVersion(ctx context.Context, version int) error
	GetMigrationVersion(ctx context.Context) (int, error)

	Backup() error
}

type Migration func(ctx context.Context, db DB) error

var Migrations = []Migration{migrationDeprecateChannels}

func migrationDeprecateChannels(ctx context.Context, db DB) error {
	list, err := db.GetChannels(ctx, nil, ChannelStateAny)
	if err != nil {
		return fmt.Errorf("failed to get channels: %w", err)
	}

	for _, ch := range list {
		ch.AcceptingActions = false
		ch.Status = ChannelStateInactive
		log.Warn().Msgf("[migration] deprecating channel %s (marked inactive)", ch.Address)

		if err := db.UpdateChannel(ctx, ch); err != nil {
			return fmt.Errorf("failed to update channel: %w", err)
		}
	}
	return nil
}

func RunMigrations(db DB) error {
	version, err := db.GetMigrationVersion(context.Background())
	if err != nil {
		return fmt.Errorf("failed to get migration version: %w", err)
	}

	if version < len(Migrations) {
		log.Info().Msgf("required migrations from %d to %d, backuping database...", version, len(Migrations))
		if err = db.Backup(); err != nil {
			return fmt.Errorf("failed to backup db: %w", err)
		}
		log.Info().Msg("backup completed, starting migrations")
	}

	for i := version; i < len(Migrations); i++ {
		log.Info().Msgf("running migration %d", i+1)
		err := db.Transaction(context.Background(), func(ctx context.Context) error {
			if err := Migrations[i](ctx, db); err != nil {
				return fmt.Errorf("failed to run migration %d: %w", i, err)
			}

			err := db.SetMigrationVersion(ctx, i+1)
			if err != nil {
				return fmt.Errorf("failed to set migration version: %w", err)
			}
			return nil
		})
		if err != nil {
			return err
		}
		log.Info().Msgf("migration %d done", i)
	}

	return nil
}
