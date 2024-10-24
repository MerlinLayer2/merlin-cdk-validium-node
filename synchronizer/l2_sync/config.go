package l2_sync

import "github.com/0xPolygonHermez/zkevm-node/dataavailability"

// Config configuration of L2 sync process
type Config struct {
	// If enabled then the L2 sync process is permitted (only for permissionless)
	Enabled bool `mapstructure:"Enabled"`
	// AcceptEmptyClosedBatches is a flag to enable or disable the acceptance of empty batches.
	// if true, the synchronizer will accept empty batches and process them.
	AcceptEmptyClosedBatches bool `mapstructure:"AcceptEmptyClosedBatches"`

	// ReprocessFullBatchOnClose if is true when a batch is closed is force to reprocess again
	ReprocessFullBatchOnClose bool `mapstructure:"ReprocessFullBatchOnClose"`

	// CheckLastL2BlockHashOnCloseBatch if is true when a batch is closed is force to check the last L2Block hash
	CheckLastL2BlockHashOnCloseBatch bool `mapstructure:"CheckLastL2BlockHashOnCloseBatch"`

	// DataSourcePriority defines the order in which L2 batch should be retrieved: local, trusted, external
	DataSourcePriority []dataavailability.DataSourcePriority `mapstructure:"DataSourcePriority"`
}
