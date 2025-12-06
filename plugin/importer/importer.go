package importer

import (
	"context"
	_ "embed"
	"errors"
	"fmt"
	"net/url"
	"reflect"
	"sync/atomic"
	"time"

	"github.com/sirupsen/logrus"
	"gopkg.in/yaml.v2"

	"github.com/algorand/go-algorand-sdk/v2/client/v2/algod"
	"github.com/algorand/go-algorand-sdk/v2/client/v2/common/models"
	"github.com/algorand/go-algorand-sdk/v2/encoding/json"
	sdk "github.com/algorand/go-algorand-sdk/v2/types"

	"github.com/algorand/conduit/conduit/data"
	"github.com/algorand/conduit/conduit/plugins"
	"github.com/algorand/conduit/conduit/plugins/importers"
)

//go:embed sample.yaml
var sampleConfig string

// metadata contains information about the plugin used for CLI helpers
var metadata = plugins.Metadata{
	Name:         PluginName,
	Description:  "Localnet importer with lead-based sync (follower mode only).",
	Deprecated:   false,
	SampleConfig: sampleConfig,
}

func init() {
	importers.Register(PluginName, importers.ImporterConstructorFunc(func() importers.Importer {
		return &localnetImporter{}
	}))
}

// localnetImporter is the object which implements the importer plugin interface
type localnetImporter struct {
	followerClient      *algod.Client // Follower node client
	leadClient          *algod.Client // Lead node client and state
	logger              *logrus.Logger
	cfg                 Config
	ctx                 context.Context
	cancel              context.CancelFunc
	genesis             *sdk.Genesis
	leadRound           atomic.Uint64 // current known lead round
	waitForRoundTimeout time.Duration
}

func (li *localnetImporter) Metadata() plugins.Metadata {
	return metadata
}

func (li *localnetImporter) Config() string {
	ret, _ := yaml.Marshal(li.cfg)
	return string(ret)
}

func (li *localnetImporter) OnComplete(input data.BlockData) error {
	// Advance the follower's sync round after successfully processing a block
	// This ensures the follower stays ahead of Conduit's processing position
	nextRound := input.Round() + 1

	// Check if lead has reached nextRound before advancing follower
	currentLeadRound := li.leadRound.Load()
	if currentLeadRound < nextRound {
		li.logger.Tracef("OnComplete(%d): skipping SetSyncRound(%d) - lead only at round %d",
			input.Round(), nextRound, currentLeadRound)
		return nil
	}

	_, err := li.followerClient.SetSyncRound(nextRound).Do(li.ctx)
	li.logger.Tracef("OnComplete(%d): called SetSyncRound(%d) err: %v", input.Round(), nextRound, err)
	return err
}

func (li *localnetImporter) Init(ctx context.Context, initProvider data.InitProvider, cfg plugins.PluginConfig, logger *logrus.Logger) error {
	li.ctx, li.cancel = context.WithCancel(ctx)
	li.logger = logger
	if err := cfg.UnmarshalConfig(&li.cfg); err != nil {
		return fmt.Errorf("unable to read configuration: %w", err)
	}

	if err := li.cfg.validateRequired(); err != nil {
		return err
	}

	// Configure lead node (source of truth)
	li.logger.Info("Configuring lead node...")

	// Parse and validate lead node URL
	leadURL, err := url.Parse(li.cfg.LeadNodeURL)
	if err != nil {
		return fmt.Errorf("invalid lead-node-url: %w", err)
	}
	if leadURL.Scheme != "http" && leadURL.Scheme != "https" {
		li.cfg.LeadNodeURL = "http://" + li.cfg.LeadNodeURL
		li.logger.Infof("Added http prefix to lead node URL: %s", li.cfg.LeadNodeURL)
	}

	// Determine lead node token (use default token if not specified)
	leadToken := li.cfg.LeadNodeToken
	if leadToken == "" {
		leadToken = li.cfg.Token
	}

	li.waitForRoundTimeout = 30 * time.Second

	// Create lead node client
	li.leadClient, err = algod.MakeClient(li.cfg.LeadNodeURL, leadToken)
	if err != nil {
		return fmt.Errorf("failed to create lead node client: %w", err)
	}

	// Configure follower node
	li.logger.Info("Configuring follower node...")

	// Parse and validate follower URL
	followerURL, err := url.Parse(li.cfg.FollowerNodeURL)
	if err != nil {
		return fmt.Errorf("invalid follower-node-url: %w", err)
	}
	if followerURL.Scheme != "http" && followerURL.Scheme != "https" {
		li.cfg.FollowerNodeURL = "http://" + li.cfg.FollowerNodeURL
		li.logger.Infof("Added http prefix to follower node URL: %s", li.cfg.FollowerNodeURL)
	}

	// Determine follower node token (use default token if not specified)
	followerToken := li.cfg.FollowerNodeToken
	if followerToken == "" {
		followerToken = li.cfg.Token
	}

	// Create follower client
	li.followerClient, err = algod.MakeClient(li.cfg.FollowerNodeURL, followerToken)
	if err != nil {
		return fmt.Errorf("failed to create follower client: %w", err)
	}

	// Fetch genesis from follower node
	genesisResponse, err := li.followerClient.GetGenesis().Do(li.ctx)
	if err != nil {
		return err
	}

	if reflect.DeepEqual(genesisResponse, models.Genesis{}) {
		return fmt.Errorf("unable to fetch genesis file from API at %s", li.cfg.FollowerNodeURL)
	}

	genesis := sdk.Genesis{
		SchemaID:    genesisResponse.Id,
		Network:     genesisResponse.Network,
		Proto:       genesisResponse.Proto,
		Allocation:  make([]sdk.GenesisAllocation, len(genesisResponse.Alloc)),
		RewardsPool: genesisResponse.Rwd,
		FeeSink:     genesisResponse.Fees,
		Timestamp:   int64(genesisResponse.Timestamp),
		Comment:     genesisResponse.Comment,
		DevMode:     genesisResponse.Devmode,
	}

	// Convert allocations
	for i, alloc := range genesisResponse.Alloc {
		var state sdk.Account
		stateBytes := json.Encode(alloc.State)
		if stateBytes == nil {
			return fmt.Errorf("error converting allocation state for address %s: %w", alloc.Addr, err)
		}
		err = json.LenientDecode(stateBytes, &state)
		if err != nil {
			return fmt.Errorf("error unmarshaling allocation state: %w", err)
		}
		genesis.Allocation[i] = sdk.GenesisAllocation{
			Address: alloc.Addr,
			Comment: alloc.Comment,
			State:   state,
		}
	}

	li.genesis = &genesis

	targetRound := uint64(initProvider.NextDBRound())
	var roundToCheck uint64 = 0
	if targetRound > 0 {
		roundToCheck = targetRound - 1
	}
	li.logger.Info("checking...")
	if li.isConduitOutOfSync(roundToCheck) {
		li.logger.Warnf(
			"WARNING: Follower is out of sync with the last successfully processed block in Conduit (round %d). "+
				"A Localnet reset may be required for syncing to continue.", roundToCheck)
	}

	return nil
}

func (li *localnetImporter) isConduitOutOfSync(checkRound uint64) bool {
	if checkRound == 0 {
		// No deltas for round 0, so check if the block is available.
		_, err := li.followerClient.Block(0).Do(li.ctx)
		if err != nil {
			li.logger.Infof("Block for round %d is unavailable on the configured node. API Response: %s", checkRound, err)
		}
		return err != nil
	}

	_, err := li.getDelta(checkRound)
	if err != nil {
		li.logger.Infof("State Delta for round %d is unavailable on the configured node. API Response: %s", checkRound, err)
	}
	return err != nil
}

func (li *localnetImporter) GetGenesis() (*sdk.Genesis, error) {
	if li.genesis != nil {
		return li.genesis, nil
	}
	return nil, fmt.Errorf("genesis not available: GetGenesis() should be called only after Init()")
}

// GetBlock implements the Importer interface, fetching a block from the follower node
// This is the main entry point for block retrieval by Conduit
func (li *localnetImporter) GetBlock(rnd uint64) (data.BlockData, error) {
	// Wait for lead to have this round available
	err := li.waitForLeadToReachRound(rnd)
	if err != nil {
		return data.BlockData{}, fmt.Errorf("GetBlock(%d): %w", rnd, err)
	}

	// Tell follower to sync to this round (idempotent - safe to call multiple times)
	li.logger.Tracef("GetBlock(%d): calling SetSyncRound", rnd)
	_, err = li.followerClient.SetSyncRound(rnd).Do(li.ctx)

	if err != nil {
		return data.BlockData{}, fmt.Errorf("GetBlock(%d): SetSyncRound failed: %w", rnd, err)
	}

	// Wait for follower to reach the round
	nodeRound, err := waitForRoundWithTimeout(li.ctx, li.logger, li.followerClient, rnd, li.waitForRoundTimeout)
	if err != nil {
		target := &SyncError{}
		if errors.As(err, &target) {
			li.logger.Warnf("GetBlock(%d) sync error: %s", rnd, err.Error())
		} else {
			err = fmt.Errorf("GetBlock(%d): waitForRoundWithTimeout failed: %w", rnd, err)
			li.logger.Error(err.Error())
		}
		return data.BlockData{}, err
	}

	// Fetch the block - getBlockInner will fetch block and delta
	blk, err := li.getBlockInner(rnd, nodeRound)
	if err != nil {
		return data.BlockData{}, err
	}

	return blk, nil
}

func (li *localnetImporter) Close() error {
	if li.cancel != nil {
		li.cancel()
	}
	return nil
}
