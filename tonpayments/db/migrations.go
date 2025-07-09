package db

import (
	"context"
	"encoding/base64"
	"fmt"
	"github.com/rs/zerolog/log"
)

type Migration func(ctx context.Context, db *DB) error

var Migrations = []Migration{migrationDeprecateChannels, migrationChangeUrgentPeerKey, migrationDeprecateChannels}

func migrationChangeUrgentPeerKey(ctx context.Context, db *DB) error {
	peers, err := db.GetUrgentPeers(ctx)
	if err != nil {
		return fmt.Errorf("failed to get urgent peers: %w", err)
	}

	for _, peer := range peers {
		if err = db.RemoveUrgentPeer(ctx, peer); err != nil {
			return fmt.Errorf("failed to remove urgent peer: %w", err)
		}

		if err = db.AddUrgentPeer(ctx, peer); err != nil {
			return fmt.Errorf("failed to add urgent peer: %w", err)
		}
		log.Warn().Msgf("[migration] migrated urgent peer %s", base64.StdEncoding.EncodeToString(peer))
	}
	return nil
}

func migrationDeprecateChannels(ctx context.Context, db *DB) error {
	list, err := db.GetChannels(ctx, nil, ChannelStateAny)
	if err != nil {
		return fmt.Errorf("failed to get channels: %w", err)
	}

	for _, ch := range list {
		if ch.Status == ChannelStateInactive && ch.AcceptingActions == false {
			continue
		}

		ch.AcceptingActions = false
		ch.Status = ChannelStateInactive
		log.Warn().Msgf("[migration] deprecating channel %s (marked inactive)", ch.Address)

		if err := db.UpdateChannel(ctx, ch); err != nil {
			return fmt.Errorf("failed to update channel: %w", err)
		}
	}
	return nil
}

func RunMigrations(db *DB) error {
	version, err := db.GetMigrationVersion(context.Background())
	if err != nil {
		return fmt.Errorf("failed to get migration version: %w", err)
	}

	if version < len(Migrations) {
		log.Info().Msgf("required migrations from %d to %d, backuping database...", version, len(Migrations))
		if err = db.storage.Backup(); err != nil {
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
		log.Info().Msgf("migration %d done", i+1)
	}

	return nil
}
