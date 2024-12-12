package iavl

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/bvinc/go-sqlite-lite/sqlite3"
	"github.com/dustin/go-humanize"
	"github.com/rs/zerolog"
)

type pruneSignal struct {
	pruneVersion int64
	checkpoints  VersionRange
}

type saveSignal struct {
	batch          *sqliteBatch
	root           *Node
	version        int64
	wantCheckpoint bool
}

type saveResult struct {
	n   int64
	err error
}

type sqlWriter struct {
	sql    *SqliteDb
	logger zerolog.Logger

	treePruneCh chan *pruneSignal
	treeCh      chan *saveSignal
	treeResult  chan *saveResult

	leafPruneCh chan *pruneSignal
	leafCh      chan *saveSignal
	leafResult  chan *saveResult
}

func (sql *SqliteDb) newSQLWriter() *sqlWriter {
	return &sqlWriter{
		sql:         sql,
		leafPruneCh: make(chan *pruneSignal),
		treePruneCh: make(chan *pruneSignal),
		leafCh:      make(chan *saveSignal),
		treeCh:      make(chan *saveSignal),
		leafResult:  make(chan *saveResult),
		treeResult:  make(chan *saveResult),
		logger:      sql.logger.With().Str("module", "write").Logger(),
	}
}

func (w *sqlWriter) start(ctx context.Context) {
	go func() {
		err := w.treeLoop(ctx)
		if err != nil {
			w.logger.Fatal().Err(err).Msg("tree loop failed")
		}
	}()
	go func() {
		err := w.leafLoop(ctx)
		if err != nil {
			w.logger.Fatal().Err(err).Msg("leaf loop failed")
		}
	}()
}

func (w *sqlWriter) leafLoop(ctx context.Context) error {
	var (
		pruneVersion     int64
		nextPruneVersion int64
		checkpoints      VersionRange
		orphanQuery      *sqlite3.Stmt
		deleteOrphan     *sqlite3.Stmt
		deleteLeaf       *sqlite3.Stmt
		pruneCount       int64
		pruneStartTime   time.Time
		err              error
	)

	beginPruneBatch := func(pruneTo int64) error {
		if err = w.sql.leafWrite.Begin(); err != nil {
			return fmt.Errorf("failed to begin leaf prune tx; %w", err)
		}
		orphanQuery, err = w.sql.leafWrite.Prepare(`SELECT version, sequence, ROWID FROM leaf_orphan WHERE at <= ?`, pruneTo)
		if err != nil {
			return fmt.Errorf("failed to prepare leaf orphan query; %w", err)
		}
		deleteOrphan, err = w.sql.leafWrite.Prepare("DELETE FROM leaf_orphan WHERE ROWID = ?")
		if err != nil {
			return fmt.Errorf("failed to prepare leaf orphan delete; %w", err)
		}
		deleteLeaf, err = w.sql.leafWrite.Prepare("DELETE FROM leaf WHERE version = ? and sequence = ?")
		if err != nil {
			return fmt.Errorf("failed to prepare leaf delete; %w", err)
		}

		return nil
	}
	startPrune := func(startPruningVersion int64) error {
		// only prune leafs to shard (checkpoint) boundaries.
		// e.g. given shards = [100, 200, 300];
		// startPruningVersion = 150; pruneTo = 100
		// startPruningVersion = 350; pruneTo = 300
		// startPruningVersion = 50; do nothing
		pruneTo := checkpoints.FindPrevious(startPruningVersion)
		if pruneTo == -1 {
			w.logger.Debug().Msgf("skipping leaf prune: requested prune version %d < first checkpoint", startPruningVersion)
			return nil
		}
		pruneVersion = pruneTo
		pruneCount = 0
		pruneStartTime = time.Now()

		w.logger.Debug().Msgf("leaf prune starting requested=%d pruneTo=%d", startPruningVersion, pruneTo)
		if err = beginPruneBatch(pruneVersion); err != nil {
			return err
		}
		return nil
	}
	commitPrune := func() error {
		if err = orphanQuery.Close(); err != nil {
			return err
		}
		orphanQuery = nil
		if err = w.sql.leafWrite.Commit(); err != nil {
			return err
		}
		w.logger.Debug().Msgf("commit leaf prune count=%s", humanize.Comma(pruneCount))
		if err = w.sql.leafWrite.Exec("PRAGMA wal_checkpoint(RESTART)"); err != nil {
			return fmt.Errorf("failed to checkpoint; %w", err)
		}

		if err = deleteLeaf.Close(); err != nil {
			return err
		}
		if err = deleteOrphan.Close(); err != nil {
			return err
		}

		return nil
	}
	stepPruning := func() error {
		hasRow, err := orphanQuery.Step()
		if err != nil {
			return fmt.Errorf("failed to step leaf orphan query; %w", err)
		}
		if hasRow {
			pruneCount++
			var (
				version  int64
				sequence int
				rowID    int64
			)
			err = orphanQuery.Scan(&version, &sequence, &rowID)
			if err != nil {
				return err
			}
			if err = deleteLeaf.Exec(version, sequence); err != nil {
				return err
			}
			if err = deleteOrphan.Exec(rowID); err != nil {
				return err
			}
			if pruneCount%pruneBatchSize == 0 {
				if err = commitPrune(); err != nil {
					return err
				}
				if err = beginPruneBatch(pruneVersion); err != nil {
					return err
				}
			}
		} else {
			if err = commitPrune(); err != nil {
				return err
			}
			err = w.sql.leafWrite.Exec("DELETE FROM leaf_delete WHERE version < ?", pruneVersion)
			if err != nil {
				return fmt.Errorf("failed to prune leaf_delete; %w", err)
			}
			w.logger.Debug().Msgf("done leaf prune count=%s dur=%s to=%d",
				humanize.Comma(pruneCount),
				time.Since(pruneStartTime).Round(time.Millisecond),
				pruneVersion,
			)
			if nextPruneVersion != 0 {
				if err = startPrune(nextPruneVersion); err != nil {
					return err
				}
				nextPruneVersion = 0
			} else {
				pruneVersion = 0
			}
		}

		return nil
	}
	saveLeaves := func(sig *saveSignal) {
		res := &saveResult{}
		res.n, res.err = sig.batch.saveLeaves()
		if sig.batch.isCheckpoint() {
			if err = w.sql.leafWrite.Exec("PRAGMA wal_checkpoint(TRUNCATE)"); err != nil {
				w.logger.Err(err).Msg("failed leaf wal_checkpoint")
			}
		}
		w.leafResult <- res
	}
	for {
		if pruneVersion != 0 {
			select {
			case sig := <-w.leafCh:
				if err = commitPrune(); err != nil {
					return fmt.Errorf("interrupt leaf prune failed in commit; %w", err)
				}
				saveLeaves(sig)
				if err = beginPruneBatch(pruneVersion); err != nil {
					return fmt.Errorf("interrupt leaf prune failed in begin; %w", err)
				}
			case sig := <-w.leafPruneCh:
				w.logger.Warn().Msgf("leaf prune signal received while pruning version=%d next=%d", pruneVersion, sig.pruneVersion)
				checkpoints = sig.checkpoints
				nextPruneVersion = sig.pruneVersion
			case <-ctx.Done():
				return nil
			default:
				err = stepPruning()
				if err != nil {
					return fmt.Errorf("failed to step pruning; %w", err)
				}
			}
		} else {
			select {
			case sig := <-w.leafCh:
				saveLeaves(sig)
			case sig := <-w.leafPruneCh:
				checkpoints = sig.checkpoints
				err = startPrune(sig.pruneVersion)
				if err != nil {
					return fmt.Errorf("failed to start leaf prune; %w", err)
				}
			case <-ctx.Done():
				return nil
			}
		}
	}
}

const pruneBatchSize = 500_000

func (w *sqlWriter) treeLoop(ctx context.Context) error {
	var (
		nextPruneVersion int64
		checkpoints      VersionRange
		pruneVersion     int64
		pruneCount       int64
		pruneStartTime   time.Time
		orphanQuery      *sqlite3.Stmt
		// TODO use a map
		deleteBranch func(shardId int64, version int64, sequence int) (err error)
		deleteOrphan *sqlite3.Stmt
	)
	beginPruneBatch := func(version int64) (err error) {
		if err = w.sql.treeWrite.Begin(); err != nil {
			return err
		}
		orphanQuery, err = w.sql.treeWrite.Prepare(
			"SELECT version, sequence, at, ROWID FROM orphan WHERE at <= ?", version)
		if err != nil {
			return fmt.Errorf("failed to prepare orphan query; %w", err)
		}
		deleteBranch = func(shardId int64, version int64, sequence int) (err error) {
			return w.sql.treeWrite.Exec(
				fmt.Sprintf("DELETE FROM tree_%d WHERE version = ? AND sequence = ?", shardId), version, sequence)
		}
		deleteOrphan, err = w.sql.treeWrite.Prepare("DELETE FROM orphan WHERE ROWID = ?")
		if err != nil {
			return fmt.Errorf("failed to prepare orphan delete; %w", err)
		}

		return err
	}
	commitPrune := func() (err error) {
		if err = orphanQuery.Close(); err != nil {
			return err
		}
		if err = deleteOrphan.Close(); err != nil {
			return err
		}
		if err = w.sql.treeWrite.Commit(); err != nil {
			return fmt.Errorf("failed to commit tree prune; %w", err)
		}
		w.logger.Debug().Msgf("commit tree prune count=%s", humanize.Comma(pruneCount))
		if err = w.sql.treeWrite.Exec("PRAGMA wal_checkpoint(RESTART)"); err != nil {
			return fmt.Errorf("failed to checkpoint; %w", err)
		}
		return nil
	}
	saveTree := func(sig *saveSignal) {
		res := &saveResult{}
		res.n, res.err = sig.batch.saveBranches()
		if res.err == nil {
			err := w.sql.SaveRoot(sig.version, sig.root, sig.wantCheckpoint)
			if err != nil {
				res.err = fmt.Errorf("failed to save root path=%s version=%d: %w", w.sql.opts.Path, sig.version, err)
			}
		}
		if sig.batch.isCheckpoint() {
			if err := w.sql.treeWrite.Exec("PRAGMA wal_checkpoint(TRUNCATE)"); err != nil {
				res.err = fmt.Errorf("failed tree checkpoint; %w", err)
			}
		}
		w.treeResult <- res
	}
	startPrune := func(startPruningVersion int64) error {
		w.logger.Debug().Msgf("tree prune to version=%d", startPruningVersion)
		pruneStartTime = time.Now()
		pruneCount = 0
		pruneVersion = startPruningVersion
		err := beginPruneBatch(pruneVersion)
		if err != nil {
			return err
		}
		return nil
	}
	stepPruning := func() error {
		hasRow, err := orphanQuery.Step()
		if err != nil {
			return fmt.Errorf("failed to step orphan query; %w", err)
		}
		if hasRow {
			pruneCount++
			var (
				version  int64
				sequence int
				at       int
				rowID    int64
			)
			err = orphanQuery.Scan(&version, &sequence, &at, &rowID)
			if err != nil {
				return err
			}
			shard, err := w.sql.getShard(version)
			if err != nil {
				return err
			}
			if err = deleteBranch(shard, version, sequence); err != nil {
				return fmt.Errorf("failed to delete from tree_%d count=%d; %w", shard, pruneCount, err)
			}
			if err = deleteOrphan.Exec(rowID); err != nil {
				return fmt.Errorf("failed to delete from orphan count=%d; %w", pruneCount, err)
			}
			if pruneCount%pruneBatchSize == 0 {
				if err = commitPrune(); err != nil {
					return err
				}
				if err = beginPruneBatch(pruneVersion); err != nil {
					return err
				}
			}
		} else {
			if err = commitPrune(); err != nil {
				return err
			}

			prevCheckpoint := checkpoints.FindPrevious(pruneVersion)
			if err = w.sql.treeWrite.Exec("DELETE FROM root WHERE version < ?", prevCheckpoint); err != nil {
				return fmt.Errorf("failed to prune root to %d; %w", prevCheckpoint, err)
			}

			w.logger.Debug().Msgf("done tree prune count=%s dur=%s to=%d",
				humanize.Comma(pruneCount),
				time.Since(pruneStartTime).Round(time.Millisecond),
				prevCheckpoint,
			)
			if nextPruneVersion != 0 {
				if err = startPrune(nextPruneVersion); err != nil {
					return err
				}
				nextPruneVersion = 0
			} else {
				pruneVersion = 0
			}
		}

		return nil
	}

	for {
		// if there is pruning in progress support interrupt and immediate continuation
		if pruneVersion != 0 {
			select {
			case sig := <-w.treeCh:
				if sig.wantCheckpoint {
					if err := commitPrune(); err != nil {
						return err
					}
					saveTree(sig)
					if err := beginPruneBatch(pruneVersion); err != nil {
						return err
					}
				} else {
					saveTree(sig)
				}
			case sig := <-w.treePruneCh:
				w.logger.Warn().Msgf("tree prune signal received while pruning version=%d next=%d", pruneVersion, sig.pruneVersion)
				checkpoints = sig.checkpoints
				nextPruneVersion = sig.pruneVersion
			case <-ctx.Done():
				return nil
			default:
				// continue pruning if no signal
				err := stepPruning()
				if err != nil {
					return err
				}
			}
		} else {
			select {
			case sig := <-w.treeCh:
				saveTree(sig)
			case sig := <-w.treePruneCh:
				checkpoints = sig.checkpoints
				err := startPrune(sig.pruneVersion)
				if err != nil {
					return err
				}
			case <-ctx.Done():
				return nil
			}
		}
	}
}

func (w *sqlWriter) saveTree(tree *Tree) error {
	saveStart := time.Now()
	parts := strings.Split(tree.sql.opts.Path, "/")

	batch := &sqliteBatch{
		sql:               tree.sql,
		queue:             tree.writeQueue,
		version:           tree.stagedVersion,
		storeLatestLeaves: tree.storeLatestLeaves,
		size:              200_000,
		logger: log.With().
			Str("module", "sqlite-batch").
			Str("path", parts[len(parts)-1]).Logger(),
	}
	saveSig := &saveSignal{batch: batch, root: tree.stagedRoot, version: tree.stagedVersion, wantCheckpoint: tree.shouldCheckpoint}
	w.treeCh <- saveSig
	w.leafCh <- saveSig
	treeResult := <-w.treeResult
	leafResult := <-w.leafResult
	dur := time.Since(saveStart)
	tree.sql.metrics.WriteDurations = append(tree.sql.metrics.WriteDurations, dur)
	tree.sql.metrics.WriteTime += dur
	tree.sql.metrics.WriteLeaves += int64(len(tree.leaves))

	//if batch.leafCount > 0 || batch.treeCount > 0 {
	//	batch.logger.Info().Msgf("saved tree version=%d leaves=%s branches=%s dur=%s",
	//		tree.stagedVersion,
	//		humanize.Comma(batch.leafCount),
	//		humanize.Comma(batch.treeCount),
	//		dur.Round(time.Millisecond),
	//	)
	//}

	err := errors.Join(treeResult.err, leafResult.err)

	return err
}

// TODO
// unify delete approach between tree and leaf. tree uses rowid range in delete, leaf issues delete for each rowid.
// which one is faster?
//
