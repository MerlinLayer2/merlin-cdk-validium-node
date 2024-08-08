package jsonrpc

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"math/big"
	"testing"

	"github.com/0xPolygonHermez/zkevm-node/etherman"
	"github.com/0xPolygonHermez/zkevm-node/etherman/smartcontracts/mockverifier"
	"github.com/0xPolygonHermez/zkevm-node/hex"
	"github.com/0xPolygonHermez/zkevm-node/jsonrpc/client"
	"github.com/0xPolygonHermez/zkevm-node/jsonrpc/types"
	"github.com/0xPolygonHermez/zkevm-node/state"
	"github.com/ethereum/go-ethereum/accounts/abi"
	"github.com/ethereum/go-ethereum/accounts/abi/bind"
	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/ethclient"
	"github.com/stretchr/testify/require"
)

type remote struct {
	conf       *Config
	rclient    *client.Client
	ethClient  *ethclient.Client
	rollupABIs map[uint64]*abi.ABI
	blockHash  common.Hash
	txindex    uint
	chainId    uint64
	forkID     uint64
}

func newRemoteTest(m *MerlinEndpoints, rclient *client.Client, blockHash common.Hash, txindex uint, chainId, forkID uint64) *remote {
	return &remote{
		conf:       &m.cfg,
		rclient:    rclient,
		ethClient:  m.etherman.OriEthClient,
		rollupABIs: m.rollupABIs,
		blockHash:  blockHash,
		txindex:    txindex,
		chainId:    chainId,
		forkID:     forkID,
	}
}

func (r *remote) getOldSnarkParamFromRemote(param *verifyBatchesTrustedAggregatorParam, sender common.Address) (interface{}, error) {
	oldBatch, err := r.rclient.BatchByNumber(context.Background(), big.NewInt(0).SetUint64(param.initNumBatch))
	if err != nil && !errors.Is(err, state.ErrNotFound) {
		return RPCErrorResponse(types.DefaultErrorCode, fmt.Sprintf("couldn't load verify batch from state by number %v", param.initNumBatch), err, true)
	}
	newBatch, err := r.rclient.BatchByNumber(context.Background(), big.NewInt(0).SetUint64(param.finalNewBatch))
	if err != nil && !errors.Is(err, state.ErrNotFound) {
		return RPCErrorResponse(types.DefaultErrorCode, fmt.Sprintf("couldn't load verify batch from state by number %v", param.initNumBatch), err, true)
	}
	return InputSnark{
		Sender:           sender,
		OldStateRoot:     oldBatch.StateRoot,
		OldAccInputHash:  oldBatch.AccInputHash,
		InitNumBatch:     param.initNumBatch,
		ChainId:          r.chainId,
		ForkID:           r.forkID,
		NewStateRoot:     newBatch.StateRoot,
		NewAccInputHash:  newBatch.AccInputHash,
		NewLocalExitRoot: newBatch.LocalExitRoot,
		FinalNewBatch:    param.finalNewBatch,
	}, nil
}

func (r *remote) getVerifyBatchesParam(blockHash common.Hash, forkID uint64) (*verifyBatchesTrustedAggregatorParam, error) {
	tx, err := r.ethClient.TransactionInBlock(context.Background(), blockHash, r.txindex)
	if err != nil {
		return nil, err
	}
	if forkID < state.FORKID_ELDERBERRY {
		return parseVerifyBatchesTrustedAggregatorOldInput(r.rollupABIs[state.FORKID_DRAGONFRUIT], tx.Data())
	}
	return parseVerifyBatchesTrustedAggregatorInput(r.rollupABIs[state.FORKID_ELDERBERRY], tx.Data())
}

func TestVerifyTestnet(t *testing.T) {
	cfg := etherman.Config{
		URL: "http://103.231.86.44:7545",
	}

	l1Config := etherman.L1Config{
		L1ChainID:                 55555,
		ZkEVMAddr:                 common.HexToAddress("0x8173da1A9d41287158E9b6E38Ca9CDabBAE6bb6B"),
		RollupManagerAddr:         common.HexToAddress("0xAefb2f4db0766F0D76c47d0dbc0A712D653cace6"),
		PolAddr:                   common.HexToAddress("0xCC1975Bd1a1A2740ea47f9090f84755817049D94"),
		GlobalExitRootManagerAddr: common.HexToAddress("0x07eb659bd996Ac74c154dfe86Ea875570647961C"),
	}

	ethermanClient, err := etherman.NewClient(cfg, l1Config, nil)
	require.NoError(t, err)

	conf := Config{
		VerifyZkProofConfigs: []*VerifyZkProofConfig{{
			ForkID:            state.FORKID_ELDERBERRY,
			VerifierAddr:      common.HexToAddress("0xf81BC46a1277EF1e7BF0AC97C990d10131154458"), //
			TrustedAggregator: common.HexToAddress("0x719647fcce805a0dae3a80c4a607c1792cff5d3c"), //
		}},
	}
	mpoints := NewMerlinEndpoints(conf, nil, ethermanClient)
	txHash := common.HexToHash("0x980343c480c0653eb0ce0b9c0787cd0bac6c64ec73300c0c87ecb0693d66d46b")
	zkp, err := mpoints.getZkProof(txHash, state.FORKID_ELDERBERRY)
	require.NoError(t, err)

	RollupData, err := mpoints.etherman.RollupManager.RollupIDToRollupData(&bind.CallOpts{Pending: false}, zkp.RollupID)
	require.NoError(t, err)
	fmt.Println("VerifierAddr", RollupData.Verifier.String(), "forkid", RollupData.ForkID)

	mver, err := mockverifier.NewMockverifier(conf.VerifyZkProofConfigs[0].VerifierAddr, ethermanClient.OriEthClient)
	require.NoError(t, err)

	isv, err := mpoints.VerifyProof(*zkp)
	require.NoError(t, err)
	require.Equal(t, true, isv)

	var pproofs [24][32]byte
	for i := range zkp.Proof {
		pproofs[i] = zkp.Proof[i]
	}
	isv, err = mver.VerifyProof(&bind.CallOpts{Pending: true}, pproofs, zkp.PubSignals)
	require.NoError(t, err)
	require.Equal(t, true, isv)
}

func TestVerifyMainnet(t *testing.T) {
	cfg := etherman.Config{
		URL: "http://18.142.49.94:8545",
	}

	l1Config := etherman.L1Config{
		L1ChainID:                 202401,
		ZkEVMAddr:                 common.HexToAddress("0xBf4B031eb29fc34E2bCb4327F9304BED3600cc46"),
		RollupManagerAddr:         common.HexToAddress("0x68DdbE6638d7514a9Ed0B9B2980B65970e532cdB"),
		PolAddr:                   common.HexToAddress("0x9e2bC6EB2c9396ccbCC66353da011b67A0ff4604"),
		GlobalExitRootManagerAddr: common.HexToAddress("0x8b97BF5C42739C375a2db080813E9b4C9A4a2c9A"),
	}

	ethermanClient, err := etherman.NewClient(cfg, l1Config, nil)
	require.NoError(t, err)

	conf := Config{
		VerifyZkProofConfigs: []*VerifyZkProofConfig{{
			ForkID:            state.FORKID_ELDERBERRY,
			VerifierAddr:      common.HexToAddress("0x65f25cED51CfDe249f307Cf6fC60A9988D249A69"), //
			TrustedAggregator: common.HexToAddress("0xe76cc099094d484e67cd7b777d22a93afc2920cc"), //
		}},
	}
	mpoints := NewMerlinEndpoints(conf, nil, ethermanClient)
	txHash := common.HexToHash("0xa20870c72bf925d832b6da488aa5242af8d1fb9223c0e87b8c345ebc2bb944b8")
	zkp, err := mpoints.getZkProof(txHash, state.FORKID_ELDERBERRY)
	require.NoError(t, err)

	print, _ := json.MarshalIndent(zkp, "", "    ")
	fmt.Println(string(print))

	RollupData, err := mpoints.etherman.RollupManager.RollupIDToRollupData(&bind.CallOpts{Pending: false}, zkp.RollupID)
	require.NoError(t, err)
	fmt.Println("VerifierAddr", RollupData.Verifier.String(), "forkid", RollupData.ForkID)

	isv, err := mpoints.VerifyProof(*zkp)
	require.NoError(t, err)
	require.Equal(t, true, isv)

	mver, err := mockverifier.NewMockverifier(conf.VerifyZkProofConfigs[0].VerifierAddr, ethermanClient.OriEthClient)
	require.NoError(t, err)

	var pproofs [24][32]byte
	for i := range zkp.Proof {
		pproofs[i] = zkp.Proof[i]
	}
	isv, err = mver.VerifyProof(&bind.CallOpts{Pending: true}, pproofs, zkp.PubSignals)
	require.NoError(t, err)
	require.Equal(t, true, isv)
}

func TestVerifyMainnetForkID5(t *testing.T) {
	cfg := etherman.Config{
		URL: "http://18.142.49.94:8545",
	}

	l1Config := etherman.L1Config{
		L1ChainID:                 202401,
		ZkEVMAddr:                 common.HexToAddress("0xBf4B031eb29fc34E2bCb4327F9304BED3600cc46"),
		RollupManagerAddr:         common.HexToAddress("0x68DdbE6638d7514a9Ed0B9B2980B65970e532cdB"),
		PolAddr:                   common.HexToAddress("0x9e2bC6EB2c9396ccbCC66353da011b67A0ff4604"),
		GlobalExitRootManagerAddr: common.HexToAddress("0x8b97BF5C42739C375a2db080813E9b4C9A4a2c9A"),
	}

	ethermanClient, err := etherman.NewClient(cfg, l1Config, nil)
	require.NoError(t, err)
	conf := Config{
		VerifyZkProofConfigs: []*VerifyZkProofConfig{{
			ForkID:            state.FORKID_DRAGONFRUIT,
			VerifierAddr:      common.HexToAddress("0x7d72cc8E89B187a93581ee44FB1884b498989A40"), //旧的 0x7d72cc8E89B187a93581ee44FB1884b498989A40  //新的 0x65f25cED51CfDe249f307Cf6fC60A9988D249A69
			TrustedAggregator: common.HexToAddress("0xe76cc099094d484e67cd7b777d22a93afc2920cc"), //
		}},
	}
	mpoints := NewMerlinEndpoints(conf, nil, ethermanClient)
	client := client.NewClient("https://rpc.merlinchain.io")

	//blockHash := common.HexToHash("0x756bd43b6d85f5fae4008cd92f7fa9198a6e6ec6b0979e7db1f323de60d522b3") //00
	blockHash := common.HexToHash("0xaa21a9814bd65c8a129e5f328e11a43ac3b7e55e38fda9d4a41f6549f6d689bc") //01
	//blockHash := common.HexToHash("0xdc6ba51440d94d69c8a4184b1a353e8bc302e6bcb0f2a4e30883b7ecd7393cc1")
	txindex := uint(0)
	chainID, err := mpoints.etherman.GetL2ChainID()
	require.NoError(t, err)
	ret := newRemoteTest(mpoints, client, blockHash, txindex, chainID, state.FORKID_DRAGONFRUIT)
	mpoints.setRemote(ret)

	zkp, err := mpoints.getZkProof(blockHash, state.FORKID_DRAGONFRUIT)
	require.NoError(t, err)

	print, _ := json.MarshalIndent(zkp, "", "    ")
	fmt.Println(string(print))

	isv, err := mpoints.VerifyProof(*zkp)
	require.NoError(t, err)
	require.Equal(t, true, isv)
}

func TestVerifyGetInputSnarkBytes(t *testing.T) {
	type testcase struct {
		input  *InputSnark
		result string
	}
	testcases := []*testcase{
		//{
		//	input: &InputSnark{
		//		Sender:           common.HexToAddress("0xe76cc099094d484e67cd7b777d22a93afc2920cc"),
		//		OldStateRoot:     common.HexToHash("0xbc26b56bbd4fa7c91c97a0e0fea120b7d26eba75daa2cc3035b5edcc2b5c6630"),
		//		OldAccInputHash:  common.HexToHash("0xab07cc71710e24d280bcd070abf25eb01b99788c985c9cd3ede196a5e9586672"),
		//		InitNumBatch:     10,
		//		ChainId:          1001,
		//		ForkID:           8,
		//		NewStateRoot:     common.HexToHash("0x97b2f0666edfff8c6eb8315c0161db5a10ae11342ba7f34da46d581bcb70e376"),
		//		NewAccInputHash:  common.HexToHash("0x0db4014d73587d6ef5f9dfabdc9a14ebafddeee91f6da5fba029f9f84bfd1631"),
		//		NewLocalExitRoot: common.HexToHash("0x0000000000000000000000000000000000000000000000000000000000000000"),
		//		FinalNewBatch:    15,
		//	},
		//	result: "0x000000000000000000000000e76cc099094d484e67cd7b777d22a93afc2920ccbc26b56bbd4fa7c91c97a0e0fea120b7d26eba75daa2cc3035b5edcc2b5c6630ab07cc71710e24d280bcd070abf25eb01b99788c985c9cd3ede196a5e9586672000000000000000000000000000000000000000000000000000000000000000a00000000000000000000000000000000000000000000000000000000000003e9000000000000000000000000000000000000000000000000000000000000000897b2f0666edfff8c6eb8315c0161db5a10ae11342ba7f34da46d581bcb70e3760db4014d73587d6ef5f9dfabdc9a14ebafddeee91f6da5fba029f9f84bfd16310000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000f",
		//},
		{
			input: &InputSnark{
				Sender:           common.HexToAddress("0xe76cc099094d484e67cd7b777d22a93afc2920cc"),
				OldStateRoot:     common.HexToHash("0xbc26b56bbd4fa7c91c97a0e0fea120b7d26eba75daa2cc3035b5edcc2b5c6630"),
				OldAccInputHash:  common.HexToHash("0xab07cc71710e24d280bcd070abf25eb01b99788c985c9cd3ede196a5e9586672"),
				InitNumBatch:     1774053,
				ChainId:          4200,
				ForkID:           8,
				NewStateRoot:     common.HexToHash("0x97b2f0666edfff8c6eb8315c0161db5a10ae11342ba7f34da46d581bcb70e376"),
				NewAccInputHash:  common.HexToHash("0x0db4014d73587d6ef5f9dfabdc9a14ebafddeee91f6da5fba029f9f84bfd1631"),
				NewLocalExitRoot: common.HexToHash("0x0000000000000000000000000000000000000000000000000000000000000000"),
				FinalNewBatch:    1774057,
			},
			result: "0xe76cc099094d484e67cd7b777d22a93afc2920ccbc26b56bbd4fa7c91c97a0e0fea120b7d26eba75daa2cc3035b5edcc2b5c6630ab07cc71710e24d280bcd070abf25eb01b99788c985c9cd3ede196a5e958667200000000001b11e50000000000001068000000000000000897b2f0666edfff8c6eb8315c0161db5a10ae11342ba7f34da46d581bcb70e3760db4014d73587d6ef5f9dfabdc9a14ebafddeee91f6da5fba029f9f84bfd1631000000000000000000000000000000000000000000000000000000000000000000000000001b11e9",
		},
	}

	for _, tc := range testcases {
		snark, err := getInputSnarkBytes(tc.input)
		require.NoError(t, err)
		fmt.Println("snark", len(hex.EncodeToString(snark)), len(tc.result), hex.EncodeToString(snark))
		require.Equal(t, tc.result, hex.EncodeToHex(snark))
	}
}
