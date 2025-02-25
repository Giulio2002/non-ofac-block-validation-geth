package blockvalidation

import (
	"math/big"
	"testing"
	"time"

	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/common/hexutil"
	"github.com/ethereum/go-ethereum/consensus/ethash"
	"github.com/ethereum/go-ethereum/core"
	"github.com/ethereum/go-ethereum/core/beacon"
	"github.com/ethereum/go-ethereum/core/rawdb"
	"github.com/ethereum/go-ethereum/core/types"
	"github.com/ethereum/go-ethereum/crypto"
	"github.com/ethereum/go-ethereum/eth"
	"github.com/ethereum/go-ethereum/eth/downloader"
	"github.com/ethereum/go-ethereum/eth/ethconfig"
	"github.com/ethereum/go-ethereum/node"
	"github.com/ethereum/go-ethereum/p2p"
	"github.com/ethereum/go-ethereum/params"
	"github.com/stretchr/testify/require"

	boostTypes "github.com/flashbots/go-boost-utils/types"
)

/* Based on catalyst API tests */

var (
	// testKey is a private key to use for funding a tester account.
	testKey, _ = crypto.HexToECDSA("b71c71a67e1177ad4e901695e1b4b9ee17ae16c6668d313eac2f96dbcda3f291")

	// testAddr is the Ethereum address of the tester account.
	testAddr = crypto.PubkeyToAddress(testKey.PublicKey)

	testBalance = big.NewInt(2e18)
)

func TestValidateBuilderSubmissionV1(t *testing.T) {
	genesis, preMergeBlocks := generatePreMergeChain(20)
	n, ethservice := startEthService(t, genesis, preMergeBlocks)
	ethservice.Merger().ReachTTD()
	defer n.Close()

	api := NewBlockValidationAPI(ethservice)
	parent := preMergeBlocks[len(preMergeBlocks)-1]

	// This EVM code generates a log when the contract is created.
	logCode := common.Hex2Bytes("60606040525b7f24ec1d3ff24c2f6ff210738839dbc339cd45a5294d85c79361016243157aae7b60405180905060405180910390a15b600a8060416000396000f360606040526008565b00")

	statedb, _ := ethservice.BlockChain().StateAt(parent.Root())
	nonce := statedb.GetNonce(testAddr)

	tx1, _ := types.SignTx(types.NewTransaction(nonce, common.Address{0x16}, big.NewInt(10), 21000, big.NewInt(2*params.InitialBaseFee), nil), types.LatestSigner(ethservice.BlockChain().Config()), testKey)
	ethservice.TxPool().AddLocal(tx1)

	cc, _ := types.SignTx(types.NewContractCreation(nonce+1, new(big.Int), 1000000, big.NewInt(2*params.InitialBaseFee), logCode), types.LatestSigner(ethservice.BlockChain().Config()), testKey)
	ethservice.TxPool().AddLocal(cc)

	tx2, _ := types.SignTx(types.NewTransaction(nonce+2, testAddr, big.NewInt(10), 21000, big.NewInt(2*params.InitialBaseFee), nil), types.LatestSigner(ethservice.BlockChain().Config()), testKey)
	ethservice.TxPool().AddLocal(tx2)

	execData, err := assembleBlock(api, parent.Hash(), &beacon.PayloadAttributesV1{
		Timestamp: parent.Time() + 5,
	})
	require.EqualValues(t, len(execData.Transactions), 3)
	require.NoError(t, err)

	payload, err := ExecutableDataToExecutionPayload(execData)
	require.NoError(t, err)

	blockRequest := &boostTypes.BuilderSubmitBlockRequest{
		Signature: boostTypes.Signature{},
		Message: &boostTypes.BidTrace{
			ParentHash: boostTypes.Hash(execData.ParentHash),
			BlockHash:  boostTypes.Hash(execData.BlockHash),
			GasLimit:   execData.GasLimit,
			GasUsed:    execData.GasUsed,
		},
		ExecutionPayload: payload,
	}
	require.ErrorContains(t, api.ValidateBuilderSubmissionV1(blockRequest), "inaccurate payment")
	blockRequest.Message.Value = boostTypes.IntToU256(191830884438530)
	require.NoError(t, api.ValidateBuilderSubmissionV1(blockRequest))

	blockRequest.Message.GasUsed = 10
	require.ErrorContains(t, api.ValidateBuilderSubmissionV1(blockRequest), "incorrect GasUsed 10, expected 98990")
	blockRequest.Message.GasUsed = execData.GasUsed

	newTestKey, _ := crypto.HexToECDSA("b71c71a67e1177ad4e901695e1b4b9ee17ae16c6668d313eac2f96dbcda3f290")
	invalidTx, err := types.SignTx(types.NewTransaction(0, common.Address{}, new(big.Int).Mul(big.NewInt(2e18), big.NewInt(10)), 19000, big.NewInt(2*params.InitialBaseFee), nil), types.LatestSigner(ethservice.BlockChain().Config()), newTestKey)
	require.NoError(t, err)

	txData, err := invalidTx.MarshalBinary()
	require.NoError(t, err)
	execData.Transactions = append(execData.Transactions, txData)

	invalidPayload, err := ExecutableDataToExecutionPayload(execData)
	require.NoError(t, err)
	invalidPayload.GasUsed = execData.GasUsed
	invalidPayload.LogsBloom = boostTypes.Bloom{}
	copy(invalidPayload.ReceiptsRoot[:], hexutil.MustDecode("0x56e81f171bcc55a6ff8345e692c0f86e5b48e01b996cadc001622fb5e363b421")[:32])
	blockRequest.ExecutionPayload = invalidPayload
	copy(blockRequest.Message.BlockHash[:], hexutil.MustDecode("0x363c62185603aa2f97e3900c9f14f946b6daec0d95314c7dcf5b6ae58ab2819c")[:32])
	require.ErrorContains(t, api.ValidateBuilderSubmissionV1(blockRequest), "could not apply tx 3", "insufficient funds for gas * price + value")
}

func generatePreMergeChain(n int) (*core.Genesis, []*types.Block) {
	db := rawdb.NewMemoryDatabase()
	config := params.AllEthashProtocolChanges
	genesis := &core.Genesis{
		Config:     config,
		Alloc:      core.GenesisAlloc{testAddr: {Balance: testBalance}},
		ExtraData:  []byte("test genesis"),
		Timestamp:  9000,
		BaseFee:    big.NewInt(params.InitialBaseFee),
		Difficulty: big.NewInt(0),
	}
	testNonce := uint64(0)
	generate := func(i int, g *core.BlockGen) {
		g.OffsetTime(5)
		g.SetExtra([]byte("test"))
		tx, _ := types.SignTx(types.NewTransaction(testNonce, common.HexToAddress("0x9a9070028361F7AAbeB3f2F2Dc07F82C4a98A02a"), big.NewInt(1), params.TxGas, big.NewInt(params.InitialBaseFee*2), nil), types.LatestSigner(config), testKey)
		g.AddTx(tx)
		testNonce++
	}
	gblock := genesis.ToBlock(db)
	engine := ethash.NewFaker()
	blocks, _ := core.GenerateChain(config, gblock, engine, db, n, generate)
	totalDifficulty := big.NewInt(0)
	for _, b := range blocks {
		totalDifficulty.Add(totalDifficulty, b.Difficulty())
	}
	config.TerminalTotalDifficulty = totalDifficulty
	return genesis, blocks
}

// startEthService creates a full node instance for testing.
func startEthService(t *testing.T, genesis *core.Genesis, blocks []*types.Block) (*node.Node, *eth.Ethereum) {
	t.Helper()

	n, err := node.New(&node.Config{
		P2P: p2p.Config{
			ListenAddr:  "0.0.0.0:0",
			NoDiscovery: true,
			MaxPeers:    25,
		}})
	if err != nil {
		t.Fatal("can't create node:", err)
	}

	ethcfg := &ethconfig.Config{Genesis: genesis, Ethash: ethash.Config{PowMode: ethash.ModeFake}, SyncMode: downloader.SnapSync, TrieTimeout: time.Minute, TrieDirtyCache: 256, TrieCleanCache: 256}
	ethservice, err := eth.New(n, ethcfg)
	if err != nil {
		t.Fatal("can't create eth service:", err)
	}
	if err := n.Start(); err != nil {
		t.Fatal("can't start node:", err)
	}
	if _, err := ethservice.BlockChain().InsertChain(blocks); err != nil {
		n.Close()
		t.Fatal("can't import test blocks:", err)
	}
	time.Sleep(500 * time.Millisecond) // give txpool enough time to consume head event

	ethservice.SetEtherbase(testAddr)
	ethservice.SetSynced()
	return n, ethservice
}

func assembleBlock(api *BlockValidationAPI, parentHash common.Hash, params *beacon.PayloadAttributesV1) (*beacon.ExecutableDataV1, error) {
	block, err := api.eth.Miner().GetSealingBlockSync(parentHash, params.Timestamp, params.SuggestedFeeRecipient, params.Random, false)
	if err != nil {
		return nil, err
	}
	return beacon.BlockToExecutableData(block), nil
}

func ExecutableDataToExecutionPayload(data *beacon.ExecutableDataV1) (*boostTypes.ExecutionPayload, error) {
	transactionData := make([]hexutil.Bytes, len(data.Transactions))
	for i, tx := range data.Transactions {
		transactionData[i] = hexutil.Bytes(tx)
	}

	baseFeePerGas := new(boostTypes.U256Str)
	err := baseFeePerGas.FromBig(data.BaseFeePerGas)
	if err != nil {
		return nil, err
	}

	return &boostTypes.ExecutionPayload{
		ParentHash:    [32]byte(data.ParentHash),
		FeeRecipient:  [20]byte(data.FeeRecipient),
		StateRoot:     [32]byte(data.StateRoot),
		ReceiptsRoot:  [32]byte(data.ReceiptsRoot),
		LogsBloom:     boostTypes.Bloom(types.BytesToBloom(data.LogsBloom)),
		Random:        [32]byte(data.Random),
		BlockNumber:   data.Number,
		GasLimit:      data.GasLimit,
		GasUsed:       data.GasUsed,
		Timestamp:     data.Timestamp,
		ExtraData:     data.ExtraData,
		BaseFeePerGas: *baseFeePerGas,
		BlockHash:     [32]byte(data.BlockHash),
		Transactions:  transactionData,
	}, nil
}
