package jsonrpc

import (
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"math/big"

	"github.com/0xPolygonHermez/zkevm-node/etherman"
	prm "github.com/0xPolygonHermez/zkevm-node/etherman/smartcontracts/polygonrollupmanager"
	"github.com/0xPolygonHermez/zkevm-node/jsonrpc/smartcontracts/verifier"
	"github.com/0xPolygonHermez/zkevm-node/jsonrpc/types"
	"github.com/0xPolygonHermez/zkevm-node/log"
	"github.com/0xPolygonHermez/zkevm-node/state"
	"github.com/ethereum/go-ethereum/accounts/abi"
	"github.com/ethereum/go-ethereum/accounts/abi/bind"
	"github.com/ethereum/go-ethereum/common"
	"github.com/jackc/pgx/v4"
)

const (
	// RFIELD is the param of verifier from calculate snark
	RFIELD                    = "21888242871839275222246405745257275088548364400416034343698204186575808495617"
	solInputLength            = 4
	decimal                   = 10
	trustedAggregatorInputNum = 8
)

var (
	// ErrNoMatchParam no match the abi param
	ErrNoMatchParam = errors.New("no match the abi param")
)

// MerlinEndpoints contains implementations for the "merlin" RPC endpoints
type MerlinEndpoints struct {
	cfg              Config
	etherman         *etherman.Client
	rollupmanagerABI *abi.ABI
	verifer          *verifier.Verifier
	state            types.StateInterface
	txMan            DBTxManager
}

// NewMerlinEndpoints creates an new instance of merlin
func NewMerlinEndpoints(cfg Config, state types.StateInterface, etherman *etherman.Client) *MerlinEndpoints {
	e := &MerlinEndpoints{
		cfg:      cfg,
		state:    state,
		etherman: etherman,
	}
	papi, err := prm.PolygonrollupmanagerMetaData.GetAbi()
	if err != nil {
		panic(fmt.Sprint("get rollupmanagerABI fail", err))
	}
	e.rollupmanagerABI = papi
	if cfg.VerifierAddr == common.HexToAddress("0x0") || cfg.TrustedAggregator == common.HexToAddress("0x0") {
		panic("the TrustedAggregator or VerifierAddr is empty")
	}
	vaddr, err := verifier.NewVerifier(cfg.VerifierAddr, etherman.GetOriginalEthClient())
	if err != nil {
		panic(fmt.Sprint("get rollupmanagerABI fail", err))
	}
	e.verifer = vaddr
	return e
}

// GetZkProof returns current zk proof
func (m *MerlinEndpoints) GetZkProof(blockNumber types.ArgUint64) (interface{}, types.Error) {
	return m.txMan.NewDbTxScope(m.state, func(ctx context.Context, dbTx pgx.Tx) (interface{}, types.Error) {
		batchNum, err := m.state.BatchNumberByL2BlockNumber(ctx, uint64(blockNumber), dbTx)
		if errors.Is(err, state.ErrNotFound) {
			return nil, nil
		} else if err != nil {
			const errorMessage = "failed to get batch number from block number"
			log.Errorf("%v: %v", errorMessage, err.Error())
			return nil, types.NewRPCError(types.DefaultErrorCode, errorMessage)
		}

		verifiedBatch, err := m.state.GetVerifiedBatch(ctx, batchNum, dbTx)
		if err != nil && !errors.Is(err, state.ErrNotFound) {
			return RPCErrorResponse(types.DefaultErrorCode, fmt.Sprintf("couldn't load verify batch from state by number %v", batchNum), err, true)
		}

		zkp, err := m.getZkProof(verifiedBatch.TxHash)
		if err != nil {
			return RPCErrorResponse(types.DefaultErrorCode, "failed to get zk prrof", err, true)
		}
		return *zkp, nil
	})
}

func (m *MerlinEndpoints) getZkProof(txHash common.Hash) (*types.ZKProof, error) {
	vbp, err := m.getVerifyBatchesParam(txHash)
	if err != nil {
		return nil, fmt.Errorf("failed getVerifyBatchesParam %v", err)
	}

	signal, err := m.getInputSnark(vbp)
	if err != nil {
		return nil, fmt.Errorf("failed getInputSnark %v", err)
	}
	var pproofs [24]common.Hash
	for i := range vbp.proof {
		pproofs[i] = vbp.proof[i]
	}
	return &types.ZKProof{
		RollupID:         vbp.rollupID,
		PendingStateNum:  vbp.pendingStateNum,
		InitNumBatch:     vbp.initNumBatch,
		FinalNewBatch:    vbp.finalNewBatch,
		NewLocalExitRoot: vbp.newLocalExitRoot,
		NewStateRoot:     vbp.newStateRoot,
		Beneficiary:      vbp.beneficiary,
		Proof:            pproofs,
		PubSignals:       [1]*big.Int{signal},
	}, nil
}

func (m *MerlinEndpoints) getVerifyBatchesParam(txHash common.Hash) (*verifyBatchesTrustedAggregatorParam, error) {
	txReceipt, err := m.etherman.GetTxReceipt(context.Background(), txHash)
	if err != nil {
		return nil, fmt.Errorf("failed getTxReceipt txHash %v %v", txHash, err)
	}
	for _, logg := range txReceipt.Logs {
		if event, err := m.etherman.RollupManager.ParseVerifyBatchesTrustedAggregator(*logg); err == nil {
			return m.getVerifyBatchesInput(event.Raw.TxHash)
		}
	}
	return nil, errors.New("failed getVerifyBatchesParam")
}
func (m *MerlinEndpoints) getVerifyBatchesInput(txHash common.Hash) (*verifyBatchesTrustedAggregatorParam, error) {
	tx, _, err := m.etherman.GetTx(context.Background(), txHash)
	if err != nil {
		return nil, err
	}
	return m.parseVerifyBatchesTrustedAggregatorInput(tx.Data())
}

type verifyBatchesTrustedAggregatorParam struct {
	rollupID         uint32
	pendingStateNum  uint64
	initNumBatch     uint64
	finalNewBatch    uint64
	newLocalExitRoot [32]byte
	newStateRoot     [32]byte
	beneficiary      common.Address
	proof            [24][32]byte
}

func (m *MerlinEndpoints) parseVerifyBatchesTrustedAggregatorInput(input []byte) (*verifyBatchesTrustedAggregatorParam, error) {
	res, err := DecodeInputParam(m.rollupmanagerABI, "verifyBatchesTrustedAggregator", input)
	if err != nil {
		return nil, err
	}
	if len(res) != trustedAggregatorInputNum {
		return nil, ErrNoMatchParam
	}
	ret := &verifyBatchesTrustedAggregatorParam{}
	var ok bool
	ret.rollupID, ok = res[0].(uint32)
	if !ok {
		return nil, ErrNoMatchParam
	}
	ret.pendingStateNum, ok = res[1].(uint64)
	if !ok {
		return nil, ErrNoMatchParam
	}
	ret.initNumBatch, ok = res[2].(uint64)
	if !ok {
		return nil, ErrNoMatchParam
	}
	ret.finalNewBatch, ok = res[3].(uint64)
	if !ok {
		return nil, ErrNoMatchParam
	}
	ret.newLocalExitRoot, ok = res[4].([32]byte)
	if !ok {
		return nil, ErrNoMatchParam
	}
	ret.newStateRoot, ok = res[5].([32]byte)
	if !ok {
		return nil, ErrNoMatchParam
	}
	ret.beneficiary, ok = res[6].(common.Address)
	if !ok {
		return nil, ErrNoMatchParam
	}
	ret.proof, ok = res[7].([24][32]byte)
	if !ok {
		return nil, ErrNoMatchParam
	}
	return ret, nil
}

func (m *MerlinEndpoints) getInputSnark(param *verifyBatchesTrustedAggregatorParam) (*big.Int, error) {
	if m.cfg.TrustedAggregator == common.HexToAddress("0x0") {
		return nil, errors.New("the TrustedAggregator is empty")
	}
	callOpts := &bind.CallOpts{Pending: false, From: m.cfg.TrustedAggregator}
	oldStateRoot, err := m.etherman.RollupManager.GetRollupBatchNumToStateRoot(callOpts, param.rollupID, param.initNumBatch)
	if err != nil {
		return nil, err
	}
	snark, err := m.etherman.RollupManager.GetInputSnarkBytes(callOpts,
		param.rollupID,
		param.initNumBatch,
		param.finalNewBatch,
		param.newLocalExitRoot,
		oldStateRoot, param.newStateRoot)
	if err != nil {
		return nil, err
	}
	hsnark := sha256.Sum256(snark)
	a := new(big.Int)
	a.SetBytes(hsnark[:])
	b := new(big.Int)
	b.SetString(RFIELD, decimal)
	c := new(big.Int)
	signal := c.Mod(a, b)
	return signal, nil
}

// VerifyProof returns VerifyProof
func (m *MerlinEndpoints) VerifyProof(zkp types.ZKProof) (interface{}, types.Error) {
	if m.verifer == nil {
		return RPCErrorResponse(types.DefaultErrorCode, "failed to verifer, the verifier address is empty", errors.New("verifier contract address is empty"), true)
	}
	var pproofs [24][32]byte
	for i, p := range zkp.Proof {
		pproofs[i] = p
	}
	isv, err := m.verifer.VerifyProof(&bind.CallOpts{Pending: false}, pproofs, zkp.PubSignals)
	if err != nil {
		return RPCErrorResponse(types.DefaultErrorCode, "failed to verify proof", err, true)
	}
	return isv, nil
}

// DecodeInputParam decode input param
func DecodeInputParam(abia *abi.ABI, methodName string, data []byte) ([]interface{}, error) {
	if len(data) <= solInputLength {
		return nil, fmt.Errorf("method %s data is nil", methodName)
	}
	method, ok := abia.Methods[methodName]
	if !ok {
		return nil, fmt.Errorf("method %s is not exist in abi", methodName)
	}
	return method.Inputs.Unpack(data[solInputLength:])
}
