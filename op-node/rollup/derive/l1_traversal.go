package derive

import (
	"context"
	"errors"
	"fmt"
	"io"

	"github.com/ethereum-optimism/optimism/op-node/eth"
	"github.com/ethereum/go-ethereum"
	"github.com/ethereum/go-ethereum/log"
)

type L1BlockRefByNumberFetcher interface {
	L1BlockRefByNumber(context.Context, uint64) (eth.L1BlockRef, error)
}

type L1Traversal struct {
	log      log.Logger
	l1Blocks L1BlockRefByNumberFetcher
	next     StageProgress
	progress Progress
}

var _ Stage = (*L1Traversal)(nil)

func NewL1Traversal(log log.Logger, l1Blocks L1BlockRefByNumberFetcher, next StageProgress) *L1Traversal {
	return &L1Traversal{
		log:      log,
		l1Blocks: l1Blocks,
		next:     next,
	}
}

func (l1s *L1Traversal) Progress() Progress {
	return l1s.progress
}

func (l1s *L1Traversal) Step(ctx context.Context, outer Progress) error {
	if !l1s.progress.Closed { // close origin and do another pipeline sweep, before we try to move to the next origin
		l1s.progress.Closed = true
		return nil
	}

	origin := l1s.progress.Origin
	nextL1Origin, err := l1s.l1Blocks.L1BlockRefByNumber(ctx, origin.Number+1)
	if errors.Is(err, ethereum.NotFound) {
		l1s.log.Debug("can't find next L1 block info (yet)", "number", origin.Number+1, "origin", origin)
		return io.EOF
	} else if err != nil {
		l1s.log.Warn("failed to find L1 block info by number", "number", origin.Number+1, "origin", origin, "err", err)
		return nil // nil, don't make the pipeline restart if the RPC fails
	}
	if l1s.progress.Origin.Hash != nextL1Origin.ParentHash {
		return fmt.Errorf("detected L1 reorg from %s to %s: %w", l1s.progress.Origin, nextL1Origin, ReorgErr)
	}
	l1s.progress.Origin = nextL1Origin
	l1s.progress.Closed = false
	return nil
}

func (l1s *L1Traversal) ResetStep(ctx context.Context, l1Fetcher L1Fetcher) error {
	l1s.progress = l1s.next.Progress()
	l1s.log.Info("completed reset of derivation pipeline", "origin", l1s.progress.Origin)
	return io.EOF
}