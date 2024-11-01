package jsonrpc

import (
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"math/big"

	"github.com/0xPolygonHermez/zkevm-node/etherman"
	oldzkevm "github.com/0xPolygonHermez/zkevm-node/etherman/smartcontracts/oldpolygonzkevm"
	prm "github.com/0xPolygonHermez/zkevm-node/etherman/smartcontracts/polygonrollupmanager"
	"github.com/0xPolygonHermez/zkevm-node/jsonrpc/smartcontracts/verifier"
	"github.com/0xPolygonHermez/zkevm-node/jsonrpc/types"
	"github.com/0xPolygonHermez/zkevm-node/state"
	"github.com/ethereum/go-ethereum/accounts/abi"
	"github.com/ethereum/go-ethereum/accounts/abi/bind"
	"github.com/ethereum/go-ethereum/common"
)

const (
	// RFIELD is the param of verifier from calculate snark
	RFIELD                       = "21888242871839275222246405745257275088548364400416034343698204186575808495617"
	solInputLength               = 4
	decimal                      = 10
	trustedAggregatorInputNum    = 8
	oldtrustedAggregatorInputNum = 6
	uint64ByteLength             = 8
)

var (
	// ErrNoMatchParam no match the abi param
	ErrNoMatchParam = errors.New("no match the abi param")
	// ErrNoMatchForkIDVerifier no match the forkid verifier
	ErrNoMatchForkIDVerifier = errors.New("no match the forkid verifier")
)

// MerlinEndpoints contains implementations for the "merlin" RPC endpoints
type MerlinEndpoints struct {
	cfg         Config
	etherman    *etherman.Client
	rollupABIs  map[uint64]*abi.ABI
	verifers    map[uint64]*verifier.Verifier
	state       types.StateInterface
	verifysConf map[uint64]*VerifyZkProofConfig
	re          RemoteT // this just for test case
}

// NewMerlinEndpoints creates an new instance of merlin
func NewMerlinEndpoints(cfg Config, statei types.StateInterface, etherman *etherman.Client) *MerlinEndpoints {
	e := &MerlinEndpoints{
		cfg:         cfg,
		state:       statei,
		etherman:    etherman,
		rollupABIs:  make(map[uint64]*abi.ABI),
		verifers:    make(map[uint64]*verifier.Verifier),
		verifysConf: make(map[uint64]*VerifyZkProofConfig),
	}
	if len(cfg.VerifyZkProofConfigs) != 0 {
		if _, err := checkMerlinZkProofConfig(&cfg); err != nil {
			panic(err.Error())
		}
		pabi, err := prm.PolygonrollupmanagerMetaData.GetAbi()
		if err != nil {
			panic(fmt.Sprint("get rollupmanagerABI fail", err))
		}
		e.rollupABIs[state.FORKID_ELDERBERRY] = pabi
		oabi, err := oldzkevm.OldpolygonzkevmMetaData.GetAbi()
		if err != nil {
			panic(fmt.Sprint("get rollupmanagerABI fail", err))
		}
		e.rollupABIs[state.FORKID_DRAGONFRUIT] = oabi
		for _, conf := range cfg.VerifyZkProofConfigs {
			e.verifysConf[conf.ForkID] = conf
			v, err := verifier.NewVerifier(conf.VerifierAddr, etherman.GetOriginalEthClient())
			if err != nil {
				panic(fmt.Sprint("get rollupmanagerABI fail", err))
			}
			if conf.ForkID < state.FORKID_ELDERBERRY {
				e.verifers[state.FORKID_DRAGONFRUIT] = v
			} else {
				e.verifers[conf.ForkID] = v // for adaptive high forkid
			}
		}
	}
	return e
}

// GetZkProof returns current zk proof
func (m *MerlinEndpoints) GetZkProof(number types.BlockNumber, withRawPubSignals bool) (interface{}, types.Error) {
	ctx := context.Background()
	if _, err := checkMerlinZkProofConfig(&m.cfg); err != nil {
		return nil, err
	}

	numeric, rpcErr := number.GetNumericBlockNumber(ctx, m.state, m.etherman, nil)
	if rpcErr != nil {
		return nil, rpcErr
	}
	if number == 0 || numeric == 0 {
		return RPCErrorResponse(types.DefaultErrorCode, "the block number = 0 or earliest, not have verify proof, should block number > 0", nil, true)
	}

	var batchNum uint64
	var err error
	if number > 0 {
		batchNum, err = m.state.BatchNumberByL2BlockNumber(ctx, uint64(number), nil)
		if err != nil {
			return RPCErrorResponse(types.DefaultErrorCode, fmt.Sprintf("failed to get batch number from block number %v, %v", number, err.Error()), nil, true)
		}
	} else {
		//get latest verify batch
		lastBatch, err := m.state.GetLastVerifiedBatch(ctx, nil)
		if err != nil {
			return RPCErrorResponse(types.DefaultErrorCode, fmt.Sprintf("failed to get the last verified batch from state %v", err.Error()), nil, true)
		}
		batchNum = lastBatch.BatchNumber
	}

	verifiedBatch, err := m.state.GetVerifiedBatch(ctx, batchNum, nil)
	if err != nil {
		return RPCErrorResponse(types.DefaultErrorCode, fmt.Sprintf("failed to load verified batch from state by batch number %v, %v", batchNum, err.Error()), nil, true)
	}

	forkID := m.state.GetForkIDByBatchNumber(batchNum)
	zkm, snark, err := m.getZkProofMeta(verifiedBatch.TxHash, forkID)
	if err != nil {
		return RPCErrorResponse(types.DefaultErrorCode, fmt.Sprintf("failed to get zk prrof by batch number %v, %v", batchNum, err.Error()), nil, true)
	}
	blks, errd := m.getVerifyBlockNumRange(zkm.initNumBatch+1, zkm.finalNewBatch)
	if err != nil {
		return nil, errd
	}
	var rawPubs *types.RawPubSignals
	if withRawPubSignals {
		rawPubs = &types.RawPubSignals{
			Snark:  snark,
			Rfield: RFIELD,
		}
	}
	return types.ZKProof{
		ForkID:        zkm.forkID,
		Proof:         zkm.proof,
		PubSignals:    zkm.pubSignals,
		RpubSignals:   rawPubs,
		StartBlockNum: blks.(blockRange).start,
		EndBlockNum:   blks.(blockRange).end,
	}, nil
}

type zkProofMeta struct {
	forkID        uint64
	proof         [24]common.Hash
	pubSignals    [1]*big.Int
	initNumBatch  uint64
	finalNewBatch uint64
}

func (m *MerlinEndpoints) getZkProofMeta(txHash common.Hash, forkID uint64) (*zkProofMeta, *types.InputSnark, error) {
	vbp, err := m.getVerifyBatchesParam(txHash, forkID)
	if err != nil {
		return nil, nil, err
	}

	snark, err := m.getInputSnark(vbp, forkID)
	if err != nil {
		return nil, nil, err
	}

	signal, err := m.getSnarkSignal(snark)
	if err != nil {
		return nil, nil, fmt.Errorf("failed getInputSnark %v", err)
	}
	var pproofs [24]common.Hash
	for i := range vbp.proof {
		pproofs[i] = vbp.proof[i]
	}
	return &zkProofMeta{
		forkID:        forkID,
		proof:         pproofs,
		pubSignals:    [1]*big.Int{signal},
		initNumBatch:  vbp.initNumBatch,
		finalNewBatch: vbp.finalNewBatch,
	}, snark, nil
}

type blockRange struct {
	start uint64
	end   uint64
}

func (m *MerlinEndpoints) getVerifyBlockNumRange(startBatch, endBatch uint64) (interface{}, types.Error) {
	ctx := context.Background()
	startBlock, err := m.state.GetL2BlocksByBatchNumber(ctx, startBatch, nil)
	if err != nil {
		return RPCErrorResponse(types.DefaultErrorCode, fmt.Sprintf("failed to get L2 blocks by batch number %v, %v", startBatch, err.Error()), nil, true)
	}
	statBlkNum := uint64(0)
	endBlkNum := uint64(0)
	if len(startBlock) != 0 {
		statBlkNum = startBlock[0].Number().Uint64()
		endBlkNum = startBlock[len(startBlock)-1].Number().Uint64()
	}
	if startBatch != endBatch {
		endBlock, err := m.state.GetL2BlocksByBatchNumber(ctx, endBatch, nil)
		if err != nil {
			return RPCErrorResponse(types.DefaultErrorCode, fmt.Sprintf("failed to get L2 blocks by batch number %v, %v", endBatch, err.Error()), nil, true)
		}
		if len(endBlock) != 0 {
			endBlkNum = endBlock[len(endBlock)-1].Number().Uint64()
		}
	}
	return blockRange{
		start: statBlkNum,
		end:   endBlkNum,
	}, nil
}

func (m *MerlinEndpoints) getVerifyBatchesParam(txHash common.Hash, forkID uint64) (*verifyBatchesTrustedAggregatorParam, error) {
	if m.re != nil {
		return m.re.getVerifyBatchesParam(txHash, forkID)
	}
	txReceipt, err := m.etherman.GetTxReceipt(context.Background(), txHash)
	if err != nil {
		return nil, fmt.Errorf("failed getTxReceipt txHash %v %v", txHash, err)
	}
	for _, logg := range txReceipt.Logs {
		if forkID < state.FORKID_ELDERBERRY {
			if _, err := m.etherman.OldZkEVM.ParseVerifyBatchesTrustedAggregator(*logg); err == nil {
				tx, _, err := m.etherman.GetTx(context.Background(), txHash)
				if err != nil {
					continue
				}
				return parseVerifyBatchesTrustedAggregatorOldInput(m.rollupABIs[state.FORKID_DRAGONFRUIT], tx.Data())
			}
		} else {
			if _, err := m.etherman.RollupManager.ParseVerifyBatchesTrustedAggregator(*logg); err == nil {
				tx, _, err := m.etherman.GetTx(context.Background(), txHash)
				if err != nil {
					continue
				}
				return parseVerifyBatchesTrustedAggregatorInput(m.rollupABIs[state.FORKID_ELDERBERRY], tx.Data())
			}
		}
	}
	return nil, errors.New("failed getVerifyBatchesParam")
}

func (m *MerlinEndpoints) getSnarkSignal(snark *types.InputSnark) (*big.Int, error) {
	d, err := getInputSnarkBytes(snark)
	if err != nil {
		return nil, err
	}
	hsnark := sha256.Sum256(d)
	a := new(big.Int)
	a.SetBytes(hsnark[:])
	b := new(big.Int)
	b.SetString(RFIELD, decimal)
	c := new(big.Int)
	signal := c.Mod(a, b)
	return signal, nil
}

func (m *MerlinEndpoints) getInputSnark(param *verifyBatchesTrustedAggregatorParam, forkID uint64) (*types.InputSnark, error) {
	if _, err := checkMerlinZkProofConfig(&m.cfg); err != nil {
		return nil, err
	}
	var conf *VerifyZkProofConfig
	var ok bool
	if forkID < state.FORKID_ELDERBERRY {
		conf, ok = m.verifysConf[state.FORKID_DRAGONFRUIT]
		if !ok {
			return nil, ErrNoMatchForkIDVerifier
		}
	} else {
		conf, ok = m.verifysConf[forkID]
		if !ok {
			return nil, ErrNoMatchForkIDVerifier
		}
	}
	return m.getInputSnarkFromLocal(param, conf.TrustedAggregator)
}

func (m *MerlinEndpoints) getInputSnarkFromLocal(param *verifyBatchesTrustedAggregatorParam, sender common.Address) (*types.InputSnark, error) {
	var rep interface{}
	var err error
	if m.re != nil {
		rep, err = m.re.getOldSnarkParamFromRemote(param, sender)
		if err != nil {
			return nil, err
		}
	} else {
		rep, err = m.getOldSnarkParamFromDB(param, sender)
		if err != nil {
			return nil, err
		}
	}
	ins, ok := rep.(types.InputSnark)
	if !ok {
		return nil, ErrNoMatchParam
	}
	return &ins, nil
}

func (m *MerlinEndpoints) getOldSnarkParamFromDB(param *verifyBatchesTrustedAggregatorParam, sender common.Address) (interface{}, error) {
	ctx := context.Background()
	oldBatch, err := m.state.GetBatchByNumber(ctx, param.initNumBatch, nil)
	if err != nil {
		return RPCErrorResponse(types.DefaultErrorCode, fmt.Sprintf("couldn't load verify batch from state by initNumBatch %v, %v", param.initNumBatch, err.Error()), nil, true)
	}
	newBatch, err := m.state.GetBatchByNumber(ctx, param.finalNewBatch, nil)
	if err != nil {
		return RPCErrorResponse(types.DefaultErrorCode, fmt.Sprintf("couldn't load verify batch from state by finalNewBatch %v, %v", param.finalNewBatch, err.Error()), nil, true)
	}

	forkID := m.state.GetForkIDByBatchNumber(param.finalNewBatch)
	chainID, err := m.etherman.GetL2ChainID()
	if err != nil {
		return RPCErrorResponse(types.DefaultErrorCode, "couldn't get l2 chainID", err, true)
	}
	return types.InputSnark{
		Sender:           sender,
		OldStateRoot:     oldBatch.StateRoot,
		OldAccInputHash:  oldBatch.AccInputHash,
		InitNumBatch:     param.initNumBatch,
		ChainId:          chainID,
		ForkID:           forkID,
		NewStateRoot:     newBatch.StateRoot,
		NewAccInputHash:  newBatch.AccInputHash,
		NewLocalExitRoot: newBatch.LocalExitRoot,
		FinalNewBatch:    param.finalNewBatch,
	}, nil
}

// VerifyZkProof verify zk proof
func (m *MerlinEndpoints) VerifyZkProof(forkID uint64, proof [24]common.Hash, pubSignals [1]*big.Int) (interface{}, types.Error) {
	if _, err := checkMerlinZkProofConfig(&m.cfg); err != nil {
		return nil, err
	}
	if forkID < state.FORKID_ELDERBERRY {
		forkID = state.FORKID_DRAGONFRUIT
	}
	verifer, ok := m.verifers[forkID]
	if !ok {
		return RPCErrorResponse(types.DefaultErrorCode, "the input param forkID is not correct", ErrNoMatchForkIDVerifier, true)
	}

	var pproofs [24][32]byte
	for i, p := range proof {
		pproofs[i] = p
	}
	isv, err := verifer.VerifyProof(&bind.CallOpts{Pending: false}, pproofs, pubSignals)
	if err != nil {
		return RPCErrorResponse(types.DefaultErrorCode, "failed to verify proof", err, true)
	}
	return isv, nil
}

func (m *MerlinEndpoints) setRemote(rt RemoteT) {
	m.re = rt
}

// RemoteT remote get interface
type RemoteT interface {
	getOldSnarkParamFromRemote(param *verifyBatchesTrustedAggregatorParam, sender common.Address) (interface{}, error)
	getVerifyBatchesParam(blockHash common.Hash, forkID uint64) (*verifyBatchesTrustedAggregatorParam, error)
	getVerifyBlockNumRange(startBatch, endBatch uint64) (interface{}, types.Error)
}

func checkMerlinZkProofConfig(cfg *Config) (interface{}, types.Error) {
	if len(cfg.VerifyZkProofConfigs) == 0 {
		return RPCErrorResponse(types.DefaultErrorCode, "the merlin zkproof config is empty", nil, true)
	}
	for _, conf := range cfg.VerifyZkProofConfigs {
		if conf.VerifierAddr == common.HexToAddress("0x0") || conf.TrustedAggregator == common.HexToAddress("0x0") {
			return RPCErrorResponse(types.DefaultErrorCode, "the TrustedAggregator or VerifierAddr is empty", nil, true)
		}
	}
	return nil, nil
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

func parseVerifyBatchesTrustedAggregatorInput(abia *abi.ABI, input []byte) (*verifyBatchesTrustedAggregatorParam, error) {
	res, err := decodeInputParam(abia, "verifyBatchesTrustedAggregator", input)
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

func parseVerifyBatchesTrustedAggregatorOldInput(abia *abi.ABI, input []byte) (*verifyBatchesTrustedAggregatorParam, error) {
	res, err := decodeInputParam(abia, "verifyBatchesTrustedAggregator", input)
	if err != nil {
		return nil, err
	}
	if len(res) != oldtrustedAggregatorInputNum {
		return nil, ErrNoMatchParam
	}
	ret := &verifyBatchesTrustedAggregatorParam{}
	var ok bool
	ret.pendingStateNum, ok = res[0].(uint64)
	if !ok {
		return nil, ErrNoMatchParam
	}
	ret.initNumBatch, ok = res[1].(uint64)
	if !ok {
		return nil, ErrNoMatchParam
	}
	ret.finalNewBatch, ok = res[2].(uint64)
	if !ok {
		return nil, ErrNoMatchParam
	}
	ret.newLocalExitRoot, ok = res[3].([32]byte)
	if !ok {
		return nil, ErrNoMatchParam
	}
	ret.newStateRoot, ok = res[4].([32]byte)
	if !ok {
		return nil, ErrNoMatchParam
	}
	ret.proof, ok = res[5].([24][32]byte)
	if !ok {
		return nil, ErrNoMatchParam
	}
	return ret, nil
}

// decodeInputParam decode input param
func decodeInputParam(abia *abi.ABI, methodName string, data []byte) ([]interface{}, error) {
	if len(data) <= solInputLength {
		return nil, fmt.Errorf("method %s data is nil", methodName)
	}
	method, ok := abia.Methods[methodName]
	if !ok {
		return nil, fmt.Errorf("method %s is not exist in abi", methodName)
	}
	return method.Inputs.Unpack(data[solInputLength:])
}

func getInputSnarkBytes(input *types.InputSnark) ([]byte, error) {
	var result []byte
	common.LeftPadBytes(big.NewInt(0).SetUint64(input.InitNumBatch).Bytes(), common.HashLength)
	result = append(result, input.Sender[:]...)
	result = append(result, input.OldStateRoot[:]...)
	result = append(result, input.OldAccInputHash[:]...)
	result = append(result, common.LeftPadBytes(big.NewInt(0).SetUint64(input.InitNumBatch).Bytes(), uint64ByteLength)...)
	result = append(result, common.LeftPadBytes(big.NewInt(0).SetUint64(input.ChainId).Bytes(), uint64ByteLength)...)
	result = append(result, common.LeftPadBytes(big.NewInt(0).SetUint64(input.ForkID).Bytes(), uint64ByteLength)...)
	result = append(result, input.NewStateRoot[:]...)
	result = append(result, input.NewAccInputHash[:]...)
	result = append(result, input.NewLocalExitRoot[:]...)
	result = append(result, common.LeftPadBytes(big.NewInt(0).SetUint64(input.FinalNewBatch).Bytes(), uint64ByteLength)...)
	return result, nil
}
