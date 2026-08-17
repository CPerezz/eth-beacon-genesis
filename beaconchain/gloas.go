package beaconchain

import (
	"fmt"

	"github.com/ethereum/go-ethereum/core"
	"github.com/ethereum/go-ethereum/core/types"
	"github.com/ethpandaops/go-eth2-client/http"
	"github.com/ethpandaops/go-eth2-client/spec"
	"github.com/ethpandaops/go-eth2-client/spec/altair"
	"github.com/ethpandaops/go-eth2-client/spec/bellatrix"
	"github.com/ethpandaops/go-eth2-client/spec/capella"
	"github.com/ethpandaops/go-eth2-client/spec/gloas"
	"github.com/ethpandaops/go-eth2-client/spec/phase0"
	"github.com/sirupsen/logrus"

	"github.com/ethpandaops/eth-beacon-genesis/beaconconfig"
	"github.com/ethpandaops/eth-beacon-genesis/beaconutils"
	"github.com/ethpandaops/eth-beacon-genesis/validators"
	dynssz "github.com/pk910/dynamic-ssz"
)

type gloasBuilder struct {
	elGenesis       *core.Genesis
	clConfig        *beaconconfig.Config
	dynSsz          *dynssz.DynSsz
	shadowForkBlock *types.Block
	validators      []*validators.Validator
}

func NewGloasBuilder(elGenesis *core.Genesis, clConfig *beaconconfig.Config) BeaconGenesisBuilder {
	return &gloasBuilder{
		elGenesis: elGenesis,
		clConfig:  clConfig,
		dynSsz:    beaconutils.GetDynSSZ(clConfig),
	}
}

func (b *gloasBuilder) SetShadowForkBlock(block *types.Block) {
	b.shadowForkBlock = block
}

func (b *gloasBuilder) AddValidators(val []*validators.Validator) {
	b.validators = append(b.validators, val...)
}

func (b *gloasBuilder) BuildState() (*spec.VersionedBeaconState, error) {
	genesisBlock := b.shadowForkBlock
	if genesisBlock == nil {
		genesisBlock = b.elGenesis.ToBlock()
	}

	genesisBlockHash := genesisBlock.Hash()

	extra := genesisBlock.Extra()
	if len(extra) > 32 {
		return nil, fmt.Errorf("extra data is %d bytes, max is %d", len(extra), 32)
	}

	depositRoot, err := beaconutils.ComputeDepositRoot(b.clConfig)
	if err != nil {
		return nil, fmt.Errorf("failed to compute deposit root: %w", err)
	}

	syncCommitteeSize := b.clConfig.GetUintDefault("SYNC_COMMITTEE_SIZE", 512)
	syncCommitteeMaskBytes := syncCommitteeSize / 8

	if syncCommitteeSize%8 != 0 {
		syncCommitteeMaskBytes++
	}

	emptyExecutionRequests := &gloas.ExecutionRequests{}

	executionRequestsRoot, err := b.dynSsz.HashTreeRoot(emptyExecutionRequests)
	if err != nil {
		return nil, fmt.Errorf("failed to compute empty execution requests root: %w", err)
	}

	// Leave this bid exactly as it is, and keep it identical to LatestExecutionPayloadBid in
	// the state below. Both the shape and the agreement were established the hard way.
	//
	// Measured against ethpandaops/{lighthouse,teku}:glamsterdam-devnet-8 with identical
	// configs, varying only these fields:
	//
	//   lighthouse LOADS the state iff this bid equals state.latest_execution_payload_bid
	//              (otherwise "Head block not found in store" at startup), and additionally
	//              only PRODUCES BLOCKS when parent_block_hash carries the execution genesis
	//              hash — it reads the execution head from there. Zero it and lighthouse
	//              sends forkchoiceUpdated with headBlockHash = 0x000…0 every slot, geth
	//              answers Invalid with no payload id, and the chain sits at genesis while
	//              every service looks RUNNING and every slot logs "empty".
	//   teku       LOADS the state iff body_root == hash_tree_root(default BeaconBlockBody()),
	//              i.e. this bid must be all-zero (AnchorPoint.fromGenesisState compares
	//              against createEmpty()). That is what initialize_beacon_state_from_eth1 in
	//              specs/phase0 mandates, and gloas does not override it.
	//
	// So no single genesis.ssz satisfies both clients: teku wants an empty bid, lighthouse
	// wants a populated one. Teku follows the spec; we run lighthouse.
	//
	// upgrade_to_gloas (specs/gloas/fork.md) sets block_hash and gas_limit instead, which
	// looks more correct and does not work — it leaves parent_block_hash zero and hits the
	// stall above. Both readings are coherent: in ePBS the latest bid describes the NEXT
	// payload, whose parent is genesis, while upgrade_to_gloas describes the LAST block.
	//
	// Nobody has hit any of this because no network starts IN gloas (glamsterdam-devnet-8
	// ships GLOAS_FORK_EPOCH: 1536); they all transition into it, leaving this builder
	// unexercised. EIP-8297 forces the issue: a binary tree needs Amsterdam at genesis,
	// hence gloas at slot 0.
	genesisBlockBody := &gloas.BeaconBlockBody{
		ETH1Data: &phase0.ETH1Data{
			BlockHash: make([]byte, 32),
		},
		SyncAggregate: &altair.SyncAggregate{
			SyncCommitteeBits: make([]byte, syncCommitteeMaskBytes),
		},
		SignedExecutionPayloadBid: &gloas.SignedExecutionPayloadBid{
			Message: &gloas.ExecutionPayloadBid{
				ParentBlockHash:       phase0.Hash32(genesisBlockHash),
				ExecutionRequestsRoot: executionRequestsRoot,
			},
			Signature: phase0.BLSSignature(make([]byte, 96)),
		},
		ParentExecutionRequests: emptyExecutionRequests,
	}

	genesisBlockBodyRoot, err := b.dynSsz.HashTreeRoot(genesisBlockBody)
	if err != nil {
		return nil, fmt.Errorf("failed to compute genesis block body root: %w", err)
	}

	genesisBuilders, genesisVals := beaconutils.SeparateBuildersFromValidators(b.validators)
	clValidators, validatorsRoot := beaconutils.GetGenesisValidators(b.clConfig, genesisVals)
	clBuilders := beaconutils.GetGenesisBuilders(b.clConfig, genesisBuilders)

	syncCommittee, err := beaconutils.GetGenesisSyncCommittee(b.clConfig, clValidators, phase0.Hash32(genesisBlockHash))
	if err != nil {
		return nil, fmt.Errorf("failed to get genesis sync committee: %w", err)
	}

	proposers, err := beaconutils.GetGenesisProposers(b.clConfig, clValidators, phase0.Hash32(genesisBlockHash))
	if err != nil {
		return nil, fmt.Errorf("failed to calculate proposer lookahead: %w", err)
	}

	ptcWindow, err := beaconutils.GetGenesisPTCWindow(b.clConfig, clValidators, phase0.Hash32(genesisBlockHash))
	if err != nil {
		return nil, fmt.Errorf("failed to calculate PTC window: %w", err)
	}

	slotsPerEpoch := b.clConfig.GetUintDefault("SLOTS_PER_EPOCH", 32)

	emptyBuilderPendingPayments := make([]*gloas.BuilderPendingPayment, slotsPerEpoch*2)
	for i := range slotsPerEpoch * 2 {
		emptyBuilderPendingPayments[i] = &gloas.BuilderPendingPayment{
			Weight: 0,
			Withdrawal: &gloas.BuilderPendingWithdrawal{
				FeeRecipient: bellatrix.ExecutionAddress{},
				Amount:       0,
				BuilderIndex: 0,
			},
		}
	}

	genesisDelay := b.clConfig.GetUintDefault("GENESIS_DELAY", 604800)
	blocksPerHistoricalRoot := b.clConfig.GetUintDefault("SLOTS_PER_HISTORICAL_ROOT", 8192)
	epochsPerSlashingVector := b.clConfig.GetUintDefault("EPOCHS_PER_SLASHINGS_VECTOR", 8192)

	minGenesisTime := b.clConfig.GetUintDefault("MIN_GENESIS_TIME", 0)
	if minGenesisTime == 0 {
		minGenesisTime = genesisBlock.Time()
	}

	genesisState := &gloas.BeaconState{
		GenesisTime:           minGenesisTime + genesisDelay,
		GenesisValidatorsRoot: validatorsRoot,
		Fork:                  GetStateForkConfig(spec.DataVersionGloas, b.clConfig),
		LatestBlockHeader: &phase0.BeaconBlockHeader{
			BodyRoot: genesisBlockBodyRoot,
		},
		BlockRoots: make([]phase0.Root, blocksPerHistoricalRoot),
		StateRoots: make([]phase0.Root, blocksPerHistoricalRoot),
		ETH1Data: &phase0.ETH1Data{
			DepositRoot: depositRoot,
			BlockHash:   genesisBlockHash[:],
		},
		JustificationBits:           make([]byte, 1),
		PreviousJustifiedCheckpoint: &phase0.Checkpoint{},
		CurrentJustifiedCheckpoint:  &phase0.Checkpoint{},
		FinalizedCheckpoint:         &phase0.Checkpoint{},
		RANDAOMixes:                 beaconutils.SeedRandomMixes(phase0.Hash32(genesisBlockHash), b.clConfig),
		Validators:                  clValidators,
		Balances:                    beaconutils.GetGenesisBalances(b.clConfig, genesisVals),
		Slashings:                   make([]phase0.Gwei, epochsPerSlashingVector),
		PreviousEpochParticipation:  make([]altair.ParticipationFlags, len(clValidators)),
		CurrentEpochParticipation:   make([]altair.ParticipationFlags, len(clValidators)),
		InactivityScores:            make([]uint64, len(clValidators)),
		CurrentSyncCommittee:        syncCommittee,
		NextSyncCommittee:           syncCommittee,
		ProposerLookahead:           proposers,
		Builders:                    clBuilders,
		// ParentBlockHash, not BlockHash, and this is load-bearing: lighthouse takes the
		// execution head from parent_block_hash here. Following upgrade_to_gloas
		// (specs/gloas/fork.md), which sets block_hash and gas_limit, leaves
		// parent_block_hash zero — and lighthouse then sends forkchoiceUpdated with
		// headBlockHash = 0x000…0 every slot. Geth answers Invalid with no payload id, no
		// block is ever built, and the chain sits at genesis looking healthy: every client
		// RUNNING, CLs peered and advancing slots, every slot "empty".
		//
		// Both readings are defensible. In ePBS the latest bid describes the NEXT payload,
		// whose parent is genesis, so parent_block_hash = genesis is coherent bookkeeping;
		// upgrade_to_gloas instead describes the LAST block. Only one of them boots.
		LatestExecutionPayloadBid: &gloas.ExecutionPayloadBid{
			ParentBlockHash:       phase0.Hash32(genesisBlockHash),
			ExecutionRequestsRoot: executionRequestsRoot,
		},
		ExecutionPayloadAvailability: beaconutils.MakeAllOnesBitvector(blocksPerHistoricalRoot),
		BuilderPendingPayments:       emptyBuilderPendingPayments,
		PayloadExpectedWithdrawals:   []*capella.Withdrawal{},
		LatestBlockHash:              phase0.Hash32(genesisBlockHash),
		PTCWindow:                    ptcWindow,
	}

	versionedState := &spec.VersionedBeaconState{
		Version: spec.DataVersionGloas,
		Gloas:   genesisState,
	}

	logrus.Infof("genesis version: gloas")
	logrus.Infof("genesis time: %v", genesisState.GenesisTime)
	logrus.Infof("genesis validators root: 0x%x", genesisState.GenesisValidatorsRoot)

	return versionedState, nil
}

func (b *gloasBuilder) Serialize(state *spec.VersionedBeaconState, contentType http.ContentType) ([]byte, error) {
	if state.Version != spec.DataVersionGloas {
		return nil, fmt.Errorf("unsupported version: %s", state.Version)
	}

	switch contentType {
	case http.ContentTypeSSZ:
		return b.dynSsz.MarshalSSZ(state.Gloas)
	case http.ContentTypeJSON:
		return state.Gloas.MarshalJSON()
	default:
		return nil, fmt.Errorf("unsupported content type: %s", contentType)
	}
}
