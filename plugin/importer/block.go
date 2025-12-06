package importer

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/sirupsen/logrus"

	"github.com/algorand/go-algorand-sdk/v2/client/v2/algod"
	"github.com/algorand/go-algorand-sdk/v2/client/v2/common"
	"github.com/algorand/go-algorand-sdk/v2/client/v2/common/models"
	"github.com/algorand/go-algorand-sdk/v2/encoding/msgpack"
	sdk "github.com/algorand/go-algorand-sdk/v2/types"

	"github.com/algorand/conduit/conduit/data"
)

// getDelta fetches the ledger state delta for a given round
func (li *localnetImporter) getDelta(rnd uint64) (sdk.LedgerStateDelta, error) {
	var delta sdk.LedgerStateDelta
	params := struct {
		Format string `url:"format,omitempty"`
	}{Format: "msgp"}
	err := (*common.Client)(li.followerClient).GetRawMsgpack(li.ctx, &delta, fmt.Sprintf("/v2/deltas/%d", rnd), params, nil)
	li.logger.Tracef("importer algod.getDelta() called /v2/deltas/%d err: %v", rnd, err)
	if err != nil {
		return sdk.LedgerStateDelta{}, err
	}

	return delta, nil
}

// waitForRoundWithTimeout blocks until the node reaches the specified round or times out
func waitForRoundWithTimeout(ctx context.Context, l *logrus.Logger, c *algod.Client, rnd uint64, to time.Duration) (uint64, error) {
	if rnd == 0 {
		return 0, nil
	}
	ctxWithTimeout, cf := context.WithTimeout(ctx, to)
	defer cf()
	status, err := c.StatusAfterBlock(rnd - 1).Do(ctxWithTimeout)
	l.Tracef("importer algod.waitForRoundWithTimeout() called StatusAfterBlock(%d) err: %v", rnd-1, err)

	if err == nil {
		// When c.StatusAfterBlock has a server-side timeout it returns the current status.
		// We use a context with timeout and the algod default timeout is 1 minute, so technically
		// with the current versions, this check should never be required.
		if rnd <= status.LastRound {
			return status.LastRound, nil
		}
		// algod's timeout should not be reached because context.WithTimeout is used
		return 0, NewSyncError(status.LastRound, rnd, fmt.Errorf("sync error, likely due to status after block timeout"))
	}

	if errors.Is(err, context.DeadlineExceeded) {
		return 0, NewSyncError(status.LastRound, rnd, fmt.Errorf("sync error, status after block timeout"))
	}

	// If there was a different error and the node is responsive, call status before returning a SyncError
	status2, err2 := c.Status().Do(ctx)
	l.Tracef("importer algod.waitForRoundWithTimeout() called Status() err: %v", err2)
	if err2 != nil {
		// If there was an error getting status, return the original error
		return 0, fmt.Errorf("unable to get status after block and status: %w", errors.Join(err, err2))
	}
	if status2.LastRound < rnd {
		return 0, NewSyncError(status2.LastRound, rnd, fmt.Errorf("status2.LastRound mismatch: %w", err))
	}

	// This is probably a connection error, not a SyncError
	return 0, fmt.Errorf("unknown errors: StatusAfterBlock(%w), Status(%w)", err, err2)
}

// getBlockInner fetches a block and its associated delta from the follower node
// This matches the original conduit algod importer structure, but without waitForRoundWithTimeout
// (which is called explicitly in GetBlock)
func (li *localnetImporter) getBlockInner(rnd uint64, nodeRound uint64) (data.BlockData, error) {
	var blockbytes []byte
	var blk data.BlockData

	blockbytes, err := li.followerClient.BlockRaw(rnd).Do(li.ctx)
	li.logger.Tracef("importer algod.GetBlock() called BlockRaw(%d) err: %v", rnd, err)
	if err != nil {
		err = fmt.Errorf("error getting block for round %d: %w", rnd, err)
		li.logger.Error(err.Error())
		return data.BlockData{}, err
	}

	tmpBlk := new(models.BlockResponse)
	err = msgpack.Decode(blockbytes, tmpBlk)
	if err != nil {
		return blk, fmt.Errorf("error decoding block for round %d: %w", rnd, err)
	}

	blk.BlockHeader = tmpBlk.Block.BlockHeader
	blk.Payset = tmpBlk.Block.Payset
	blk.Certificate = tmpBlk.Cert

	// Fetch the state delta (follower mode always has deltas)
	// Round 0 has no delta associated with it
	if rnd != 0 {
		var delta sdk.LedgerStateDelta
		delta, err = li.getDelta(rnd)
		if err != nil {
			if nodeRound < rnd {
				err = fmt.Errorf("ledger state delta not found: node round (%d) is behind required round (%d), ensure follower node has its sync round set to the required round: %w", nodeRound, rnd, err)
			} else {
				err = fmt.Errorf("ledger state delta not found: node round (%d), required round (%d): verify follower node configuration and ensure follower node has its sync round set to the required round, re-deploying the follower node may be necessary: %w", nodeRound, rnd, err)
			}
			li.logger.Error(err.Error())
			return data.BlockData{}, err
		}
		blk.Delta = &delta
	}

	return blk, err
}

// Calls waitForRoundWithTimeout and updates leadRound if successful.
func (li *localnetImporter) waitForLeadRoundAndUpdateState(rnd uint64, timeout time.Duration) (uint64, error) {
	leadRound, err := waitForRoundWithTimeout(li.ctx, li.logger, li.leadClient, rnd, timeout)
	if err == nil && leadRound > 0 {
		// Use CAS loop to safely update leadRound
		for {
			current := li.leadRound.Load()
			if leadRound <= current {
				break // Already at this round
			}
			if li.leadRound.CompareAndSwap(current, leadRound) {
				li.logger.Tracef("Updated lead round to %d", leadRound)
				break
			}
			// CAS failed, another goroutine updated it - loop and check again
		}
	}
	return leadRound, err
}

// blocks until the lead node has reached or passed the target round
func (li *localnetImporter) waitForLeadToReachRound(rnd uint64) error {
	currentLeadRound := li.leadRound.Load()

	// If lead is at or past the target round, return
	if currentLeadRound >= rnd {
		li.logger.Tracef("Lead already at round %d (requested: %d)", currentLeadRound, rnd)
		return nil
	}

	li.logger.Debugf("Waiting for lead to reach round %d (currently at %d)", rnd, currentLeadRound)

	for {
		leadRound, err := li.waitForLeadRoundAndUpdateState(rnd, li.waitForRoundTimeout)
		if err == nil && leadRound >= rnd {
			li.logger.Debugf("Lead reached round %d (requested: %d)", leadRound, rnd)
			return nil
		}

		// Sync error in the localnet lead context means we are at the tip of the chain and need to wait for the next block
		var syncErr *SyncError
		if err != nil && !errors.As(err, &syncErr) {
			return err
		}
		li.logger.Debugf("Lead did not reach round %d. Retrying.", rnd)
		// Continue loop to retry
	}
}
