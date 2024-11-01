package client

import (
	"context"
	"encoding/json"
	"math/big"

	"github.com/0xPolygonHermez/zkevm-node/hex"
	"github.com/0xPolygonHermez/zkevm-node/jsonrpc/types"
	"github.com/ethereum/go-ethereum/common"
)

// BatchNumber returns the latest batch number
func (c *Client) BatchNumber(ctx context.Context) (uint64, error) {
	response, err := JSONRPCCall(c.url, "zkevm_batchNumber")
	if err != nil {
		return 0, err
	}

	if response.Error != nil {
		return 0, response.Error.RPCError()
	}

	var result string
	err = json.Unmarshal(response.Result, &result)
	if err != nil {
		return 0, err
	}

	bigBatchNumber := hex.DecodeBig(result)
	batchNumber := bigBatchNumber.Uint64()

	return batchNumber, nil
}

// BatchByNumber returns a batch from the current canonical chain. If number is nil, the
// latest known batch is returned.
func (c *Client) BatchByNumber(ctx context.Context, number *big.Int) (*types.Batch, error) {
	bn := types.LatestBatchNumber
	if number != nil {
		bn = types.BatchNumber(number.Int64())
	}
	response, err := JSONRPCCall(c.url, "zkevm_getBatchByNumber", bn.StringOrHex(), true)
	if err != nil {
		return nil, err
	}

	if response.Error != nil {
		return nil, response.Error.RPCError()
	}

	var result *types.Batch
	err = json.Unmarshal(response.Result, &result)
	if err != nil {
		return nil, err
	}

	return result, nil
}

// BatchesByNumbers returns batches from the current canonical chain by batch numbers. If the list is empty, the last
// known batch is returned as a list.
func (c *Client) BatchesByNumbers(ctx context.Context, numbers []*big.Int) ([]*types.BatchData, error) {
	return c.batchesByNumbers(ctx, numbers, "zkevm_getBatchDataByNumbers")
}

// ForcedBatchesByNumbers returns forced batches data.
func (c *Client) ForcedBatchesByNumbers(ctx context.Context, numbers []*big.Int) ([]*types.BatchData, error) {
	return c.batchesByNumbers(ctx, numbers, "zkevm_getForcedBatchDataByNumbers")
}

// BatchesByNumbers returns batches from the current canonical chain by batch numbers. If the list is empty, the last
// known batch is returned as a list.
func (c *Client) batchesByNumbers(_ context.Context, numbers []*big.Int, method string) ([]*types.BatchData, error) {
	batchNumbers := make([]types.BatchNumber, 0, len(numbers))
	for _, n := range numbers {
		batchNumbers = append(batchNumbers, types.BatchNumber(n.Int64()))
	}
	if len(batchNumbers) == 0 {
		batchNumbers = append(batchNumbers, types.LatestBatchNumber)
	}

	response, err := JSONRPCCall(c.url, method, &types.BatchFilter{Numbers: batchNumbers})
	if err != nil {
		return nil, err
	}

	if response.Error != nil {
		return nil, response.Error.RPCError()
	}

	var result *types.BatchDataResult
	err = json.Unmarshal(response.Result, &result)
	if err != nil {
		return nil, err
	}

	return result.Data, nil
}

// ExitRootsByGER returns the exit roots accordingly to the provided Global Exit Root
func (c *Client) ExitRootsByGER(ctx context.Context, globalExitRoot common.Hash) (*types.ExitRoots, error) {
	response, err := JSONRPCCall(c.url, "zkevm_getExitRootsByGER", globalExitRoot.String())
	if err != nil {
		return nil, err
	}

	if response.Error != nil {
		return nil, response.Error.RPCError()
	}

	var result *types.ExitRoots
	err = json.Unmarshal(response.Result, &result)
	if err != nil {
		return nil, err
	}

	return result, nil
}

// GetLatestGlobalExitRoot returns the latest global exit root
func (c *Client) GetLatestGlobalExitRoot(ctx context.Context) (common.Hash, error) {
	response, err := JSONRPCCall(c.url, "zkevm_getLatestGlobalExitRoot")
	if err != nil {
		return common.Hash{}, err
	}

	if response.Error != nil {
		return common.Hash{}, response.Error.RPCError()
	}

	var result string
	err = json.Unmarshal(response.Result, &result)
	if err != nil {
		return common.Hash{}, err
	}

	return common.HexToHash(result), nil
}
