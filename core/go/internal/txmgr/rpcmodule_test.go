/*
 * Copyright © 2024 Kaleido, Inc.
 *
 * Licensed under the Apache License, Version 2.0 (the "License"); you may not use this file except in compliance with
 * the License. You may obtain a copy of the License at
 *
 * http://www.apache.org/licenses/LICENSE-2.0
 *
 * Unless required by applicable law or agreed to in writing, software distributed under the License is distributed on
 * an "AS IS" BASIS, WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied. See the License for the
 * specific language governing permissions and limitations under the License.
 *
 * SPDX-License-Identifier: Apache-2.0
 */

package txmgr

import (
	"context"
	"crypto/rand"
	"fmt"
	"math/big"
	"testing"

	"github.com/google/uuid"
	"github.com/hyperledger/firefly-signer/pkg/abi"
	"github.com/kaleido-io/paladin/config/pkg/confutil"
	"github.com/kaleido-io/paladin/config/pkg/pldconf"
	"github.com/kaleido-io/paladin/core/internal/components"
	"github.com/kaleido-io/paladin/core/mocks/componentmocks"
	"github.com/kaleido-io/paladin/core/pkg/ethclient"

	"github.com/kaleido-io/paladin/toolkit/pkg/algorithms"
	"github.com/kaleido-io/paladin/toolkit/pkg/pldapi"
	"github.com/kaleido-io/paladin/toolkit/pkg/query"
	"github.com/kaleido-io/paladin/toolkit/pkg/rpcclient"
	"github.com/kaleido-io/paladin/toolkit/pkg/rpcserver"
	"github.com/kaleido-io/paladin/toolkit/pkg/tktypes"
	"github.com/kaleido-io/paladin/toolkit/pkg/verifiers"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/mock"
	"github.com/stretchr/testify/require"
)

func newTestTransactionManagerWithRPC(t *testing.T, init ...func(*pldconf.TxManagerConfig, *mockComponents)) (context.Context, string, *txManager, func()) {
	ctx, txm, txmDone := newTestTransactionManager(t, true, init...)

	rpcServer, err := rpcserver.NewRPCServer(ctx, &pldconf.RPCServerConfig{
		HTTP: pldconf.RPCServerConfigHTTP{
			HTTPServerConfig: pldconf.HTTPServerConfig{
				Port:            confutil.P(0),
				ShutdownTimeout: confutil.P("0"),
			},
		},
		WS: pldconf.RPCServerConfigWS{Disabled: true},
	})
	require.NoError(t, err)

	rpcServer.Register(txm.rpcModule)
	rpcServer.Register(txm.debugRpcModule)

	err = rpcServer.Start()
	require.NoError(t, err)

	return ctx, fmt.Sprintf("http://%s", rpcServer.HTTPAddr()), txm, func() {
		txmDone()
		rpcServer.Stop()
	}

}

func mockResolveKeyOKThenFail(t *testing.T, mc *mockComponents, identifier string, senderAddr *tktypes.EthAddress) {
	kr := mockKeyResolverForFail(t, mc)
	kr.On("ResolveKey", identifier, algorithms.ECDSA_SECP256K1, verifiers.ETH_ADDRESS).
		Return(&pldapi.KeyMappingAndVerifier{Verifier: &pldapi.KeyVerifier{
			Verifier: senderAddr.String(),
		}}, nil)
}

func mockResolveKey(t *testing.T, mc *mockComponents, identifier string, senderAddr *tktypes.EthAddress) {
	kr := mockKeyResolver(t, mc)
	kr.On("ResolveKey", identifier, algorithms.ECDSA_SECP256K1, verifiers.ETH_ADDRESS).
		Return(&pldapi.KeyMappingAndVerifier{Verifier: &pldapi.KeyVerifier{
			Verifier: senderAddr.String(),
		}}, nil)
}

func mockSubmitPublicTxOk(t *testing.T, senderAddr *tktypes.EthAddress) func(tmc *pldconf.TxManagerConfig, mc *mockComponents) {
	return func(tmc *pldconf.TxManagerConfig, mc *mockComponents) {
		mockResolveKey(t, mc, "sender1", senderAddr)
		mc.publicTxMgr.On("ValidateTransaction", mock.Anything, mock.Anything).Return(nil)
		mc.publicTxMgr.On("WriteNewTransactions", mock.Anything, mock.Anything, mock.Anything).Return([]*pldapi.PublicTx{
			{LocalID: confutil.P(uint64(12345))},
		}, nil)
	}
}

func TestPublicTransactionLifecycle(t *testing.T) {

	senderAddr := tktypes.RandAddress()
	var publicTxns map[uuid.UUID][]*pldapi.PublicTx
	ctx, url, tmr, done := newTestTransactionManagerWithRPC(t,
		mockSubmitPublicTxOk(t, senderAddr),
		mockQueryPublicTxForTransactions(func(ids []uuid.UUID, jq *query.QueryJSON) (map[uuid.UUID][]*pldapi.PublicTx, error) {
			return publicTxns, nil
		}),
		func(tmc *pldconf.TxManagerConfig, mc *mockComponents) {
			mc.keyManager.On("ResolveEthAddressNewDatabaseTX", mock.Anything, "sender1").Return(senderAddr, nil) // used in call

			unconnected := ethclient.NewUnconnectedRPCClient(context.Background(), &pldconf.EthClientConfig{}, 0)
			mc.ethClientFactory.On("HTTPClient").Return(unconnected)
		},
	)
	defer done()

	rpcClient, err := rpcclient.NewHTTPClient(ctx, &pldconf.HTTPClientConfig{URL: url})
	require.NoError(t, err)

	sampleABI := abi.ABI{
		{Type: abi.Constructor, Inputs: abi.ParameterArray{
			{Type: "uint256"}, // unname param works with array input
		}},
		{Type: abi.Function, Name: "set", Inputs: abi.ParameterArray{
			{Type: "uint256"}, // named where we are using an object input
		}},
		{Type: abi.Function, Name: "get", Outputs: abi.ParameterArray{
			{Type: "uint256"},
		}},
		{Type: abi.Error, Name: "BadValue", Inputs: abi.ParameterArray{
			{Type: "uint256"},
		}},
		{Type: abi.Event, Name: "Updated", Inputs: abi.ParameterArray{
			{Type: "uint256", Name: "value", Indexed: true},
		}},
	}

	// Submit in a public deploy with array encoded params and bytecode
	tx0ID := uuid.New()
	var tx1ID uuid.UUID
	err = rpcClient.CallRPC(ctx, &tx1ID, "ptx_sendTransaction", &pldapi.TransactionInput{
		ABI:       sampleABI,
		Bytecode:  tktypes.MustParseHexBytes("0x11223344"),
		DependsOn: []uuid.UUID{tx0ID},
		TransactionBase: pldapi.TransactionBase{
			IdempotencyKey: "tx1",
			From:           "sender1",
			Type:           pldapi.TransactionTypePublic.Enum(),
			Data:           tktypes.RawJSON(`[12345]`),
		},
	})
	require.NoError(t, err)
	assert.NotEqual(t, uuid.UUID{}, tx1ID)

	// Mock up the existence of the public TXs
	publicTxns = map[uuid.UUID][]*pldapi.PublicTx{
		tx1ID: {
			{
				From:  *senderAddr,
				Nonce: confutil.P(tktypes.HexUint64(111222333)),
			},
		},
	}

	// Get it back by idempotency key
	var tx *pldapi.Transaction
	err = rpcClient.CallRPC(ctx, &tx, "ptx_getTransactionByIdempotencyKey", "not_submitted")
	require.NoError(t, err)
	assert.Nil(t, tx)
	err = rpcClient.CallRPC(ctx, &tx, "ptx_getTransactionByIdempotencyKey", "tx1")
	require.NoError(t, err)
	assert.Equal(t, tx1ID, *tx.ID)

	// Query them back
	var txns []*pldapi.TransactionFull
	err = rpcClient.CallRPC(ctx, &txns, "ptx_queryTransactionsFull", query.NewQueryBuilder().Limit(1).Query())
	require.NoError(t, err)
	assert.Len(t, txns, 1)
	assert.Equal(t, tx1ID, *txns[0].ID)
	assert.Equal(t, tx0ID, txns[0].DependsOn[0])
	assert.Equal(t, `{"0":"12345"}`, txns[0].Data.String())
	assert.Equal(t, "(uint256)", txns[0].Function)
	assert.Equal(t, *senderAddr, txns[0].Public[0].From)
	assert.Equal(t, uint64(111222333), txns[0].Public[0].Nonce.Uint64())

	// Check full=false
	txns = nil
	err = rpcClient.CallRPC(ctx, &txns, "ptx_queryTransactions", query.NewQueryBuilder().Limit(1).Query())
	require.NoError(t, err)
	assert.Len(t, txns, 1)

	// Get the stored ABIs to check we found it
	var abis []*pldapi.StoredABI
	err = rpcClient.CallRPC(ctx, &abis, "ptx_queryStoredABIs", query.NewQueryBuilder().Limit(1).Query())
	require.NoError(t, err)
	assert.Len(t, abis, 1)

	// Upsert the same ABI and check we get the same hash
	var abiHash tktypes.Bytes32
	err = rpcClient.CallRPC(ctx, &abiHash, "ptx_storeABI", sampleABI)
	require.NoError(t, err)
	assert.Equal(t, abiHash, abis[0].Hash)
	assert.Equal(t, abiHash, *txns[0].ABIReference)

	// Get it directly by ID
	var abiGet *pldapi.StoredABI
	err = rpcClient.CallRPC(ctx, &abiGet, "ptx_getStoredABI", abiHash)
	require.NoError(t, err)
	assert.Equal(t, abiHash, abiGet.Hash)

	// Null on not found is the consistent ethereum pattern
	var abiNotFound *pldapi.StoredABI
	err = rpcClient.CallRPC(ctx, &abiNotFound, "ptx_getStoredABI", tktypes.Bytes32(tktypes.RandBytes(32)))
	require.NoError(t, err)
	assert.Nil(t, abiNotFound)

	// Submit in a public invoke using that same ABI referring to the function
	tx2Input := &pldapi.TransactionInput{
		DependsOn: []uuid.UUID{tx1ID},
		TransactionBase: pldapi.TransactionBase{
			ABIReference:   &abiHash,
			IdempotencyKey: "tx2",
			Type:           pldapi.TransactionTypePublic.Enum(),
			Data:           tktypes.RawJSON(`{"0": 123456789012345678901234567890}`), // nice big JSON number to deal with
			Function:       "set(uint256)",
			From:           "sender1",
			To:             tktypes.MustEthAddress(tktypes.RandHex(20)),
		},
	}
	var txIDs []uuid.UUID
	err = rpcClient.CallRPC(ctx, &txIDs, "ptx_sendTransactions", []*pldapi.TransactionInput{tx2Input})
	assert.NoError(t, err)
	tx2ID := txIDs[0]
	var tx2 *pldapi.TransactionFull
	err = rpcClient.CallRPC(ctx, &tx2, "ptx_getTransactionFull", tx2ID)
	require.NoError(t, err)
	assert.Equal(t, tx2ID, *tx2.ID)
	assert.Equal(t, "set(uint256)", tx2.Function)
	err = rpcClient.CallRPC(ctx, &tx2, "ptx_getTransaction", tx2ID)
	require.NoError(t, err)
	assert.Equal(t, tx2ID, *tx2.ID)

	// Submit again and check we get the right error with the ID
	err = rpcClient.CallRPC(ctx, &txIDs, "ptx_sendTransactions", []*pldapi.TransactionInput{tx2Input})
	assert.Regexp(t, fmt.Sprintf("PD012220.*tx2=%s", tx2ID), err)

	// Null on not found is the consistent ethereum pattern
	var txNotFound *pldapi.Transaction
	err = rpcClient.CallRPC(ctx, &txns, "ptx_getTransaction", uuid.New())
	require.NoError(t, err)
	assert.Nil(t, txNotFound)

	// Finalize the deploy as a success
	txHash1 := tktypes.Bytes32(tktypes.RandBytes(32))
	blockNumber1 := int64(12345)
	err = tmr.FinalizeTransactions(ctx, tmr.p.DB(), []*components.ReceiptInput{
		{
			TransactionID: tx1ID,
			ReceiptType:   components.RT_Success,
			OnChain: tktypes.OnChainLocation{
				Type:            tktypes.OnChainTransaction,
				TransactionHash: txHash1,
				BlockNumber:     blockNumber1,
			},
		},
	})
	require.NoError(t, err)

	// We should get that back with full
	var txWithReceipt *pldapi.TransactionFull
	err = rpcClient.CallRPC(ctx, &txWithReceipt, "ptx_getTransactionFull", tx1ID)
	require.NoError(t, err)
	require.True(t, txWithReceipt.Receipt.Success)
	require.Equal(t, txHash1, *txWithReceipt.Receipt.TransactionHash)
	require.Equal(t, blockNumber1, txWithReceipt.Receipt.BlockNumber)
	require.Nil(t, txWithReceipt.Receipt.RevertData)

	// Select just pending transactions
	var pendingTransactionFull []*pldapi.TransactionFull
	err = rpcClient.CallRPC(ctx, &pendingTransactionFull, "ptx_queryPendingTransactions", query.NewQueryBuilder().Limit(100).Query(), true)
	require.NoError(t, err)
	require.Len(t, pendingTransactionFull, 1)
	require.Equal(t, tx2ID, *pendingTransactionFull[0].ID)
	require.Len(t, pendingTransactionFull[0].DependsOn, 1)
	var pendingTransactions []*pldapi.Transaction
	err = rpcClient.CallRPC(ctx, &pendingTransactions, "ptx_queryPendingTransactions", query.NewQueryBuilder().Limit(100).Query(), false)
	require.NoError(t, err)
	require.Len(t, pendingTransactions, 1)

	// Finalize the invoke as a revert with an encoded error
	txHash2 := tktypes.Bytes32(tktypes.RandBytes(32))
	blockNumber2 := int64(12345)
	revertData, err := sampleABI.Errors()["BadValue"].EncodeCallDataValuesCtx(ctx, []any{12345})
	require.NoError(t, err)
	err = tmr.FinalizeTransactions(ctx, tmr.p.DB(), []*components.ReceiptInput{
		{
			TransactionID: tx2ID,
			ReceiptType:   components.RT_FailedOnChainWithRevertData,
			OnChain: tktypes.OnChainLocation{
				Type:            tktypes.OnChainTransaction,
				TransactionHash: txHash2,
				BlockNumber:     blockNumber2,
			},
			RevertData: revertData,
		},
	})
	require.NoError(t, err)

	var de *pldapi.ABIDecodedData
	err = rpcClient.CallRPC(ctx, &de, "ptx_decodeError", tktypes.HexBytes(revertData), tktypes.DefaultJSONFormatOptions)
	require.NoError(t, err)
	require.Equal(t, `BadValue("12345")`, de.Summary)

	// Ask for the receipt directly
	var txReceipt *pldapi.TransactionReceipt
	err = rpcClient.CallRPC(ctx, &txReceipt, "ptx_getTransactionReceipt", tx2ID)
	require.NoError(t, err)
	require.NotNil(t, txReceipt)
	require.False(t, txReceipt.Success)
	require.Equal(t, txHash2, *txReceipt.TransactionHash)
	require.Equal(t, blockNumber2, txReceipt.BlockNumber)
	require.Equal(t, tktypes.HexBytes(revertData).String(), txReceipt.RevertData.String())
	require.Equal(t, `PD012216: Transaction reverted BadValue("12345")`, txReceipt.FailureMessage)

	// Select just success receipts
	var successReceipts []*pldapi.TransactionReceipt
	err = rpcClient.CallRPC(ctx, &successReceipts, "ptx_queryTransactionReceipts", query.NewQueryBuilder().Limit(100).Equal("success", true).Query())
	require.NoError(t, err)
	require.Len(t, successReceipts, 1)
	assert.Equal(t, successReceipts[0].ID, tx1ID)

	// Get the dependency in the middle of the chain 0, 1, 2 to see both sides
	var tx1Deps *pldapi.TransactionDependencies
	err = rpcClient.CallRPC(ctx, &tx1Deps, "ptx_getTransactionDependencies", tx1ID)
	require.NoError(t, err)
	assert.Equal(t, []uuid.UUID{tx0ID}, tx1Deps.DependsOn)
	assert.Equal(t, []uuid.UUID{tx2ID}, tx1Deps.PrereqOf)

	var resJSON tktypes.RawJSON
	err = rpcClient.CallRPC(ctx, &resJSON, "ptx_call", &pldapi.TransactionCall{
		TransactionInput: pldapi.TransactionInput{
			TransactionBase: pldapi.TransactionBase{
				IdempotencyKey: "tx2",
				Type:           pldapi.TransactionTypePublic.Enum(),
				Data:           tktypes.RawJSON(`{"0": 123456789012345678901234567890}`),
				Function:       "get()",
				From:           "sender1",
				To:             tktypes.MustEthAddress(tktypes.RandHex(20)),
			},
			ABI: sampleABI,
		},
	})
	assert.Regexp(t, "PD011517", err) // means we got all the way to the unconnected client

	// Decode a sample call using the stored and shredded ABIs
	data, err := sampleABI.Functions()["set"].EncodeCallDataJSON([]byte(`{"0": 123456789012345678901234567890}`))
	require.NoError(t, err)
	var decodedCall *pldapi.ABIDecodedData
	err = rpcClient.CallRPC(ctx, &decodedCall, "ptx_decodeCall", tktypes.HexBytes(data), "")
	require.NoError(t, err)
	require.JSONEq(t, `{"0": "123456789012345678901234567890"}`, decodedCall.Data.String())

	// Decode a sample event using the stored and shredded ABIs
	valueEncoded, err := (&abi.ParameterArray{{Type: "uint256"}}).EncodeABIDataJSON([]byte(`["123456789012345678901234567890"]`))
	require.NoError(t, err)
	var decodedEvent *pldapi.ABIDecodedData
	err = rpcClient.CallRPC(ctx, &decodedEvent, "ptx_decodeEvent", []string{
		sampleABI.Events()["Updated"].SignatureHashBytes().String(), // topic 0
		tktypes.Bytes32(valueEncoded).String(),                      // indexed integer, so can just directly pass data
	}, "0x", "")
	require.NoError(t, err)
	require.JSONEq(t, `{"value": "123456789012345678901234567890"}`, decodedEvent.Data.String())

}

func TestPublicTransactionPassthroughQueries(t *testing.T) {

	nonce, _ := rand.Int(rand.Reader, big.NewInt(10000000))
	tx := &pldapi.PublicTxWithBinding{
		PublicTx: &pldapi.PublicTx{
			From:  tktypes.EthAddress(tktypes.RandBytes(20)),
			Nonce: confutil.P(tktypes.HexUint64(nonce.Uint64())),
		},
		PublicTxBinding: pldapi.PublicTxBinding{Transaction: uuid.New(), TransactionType: pldapi.TransactionTypePublic.Enum()},
	}
	var mockQuery func(jq *query.QueryJSON) ([]*pldapi.PublicTxWithBinding, error)
	var mockGetByHash func(hash tktypes.Bytes32) (*pldapi.PublicTxWithBinding, error)
	ctx, url, _, done := newTestTransactionManagerWithRPC(t,
		mockQueryPublicTxWithBindings(func(jq *query.QueryJSON) ([]*pldapi.PublicTxWithBinding, error) { return mockQuery(jq) }),
		mockGetPublicTransactionForHash(func(hash tktypes.Bytes32) (*pldapi.PublicTxWithBinding, error) { return mockGetByHash(hash) }),
	)
	defer done()

	rpcClient, err := rpcclient.NewHTTPClient(ctx, &pldconf.HTTPClientConfig{URL: url})
	require.NoError(t, err)

	// Simple query
	sampleTxns := []*pldapi.PublicTxWithBinding{tx}
	mockQuery = func(_ *query.QueryJSON) ([]*pldapi.PublicTxWithBinding, error) { return sampleTxns, nil }
	var txns []*pldapi.PublicTxWithBinding
	err = rpcClient.CallRPC(ctx, &txns, "ptx_queryPublicTransactions", query.NewQueryBuilder().Limit(100).Query())
	require.NoError(t, err)
	require.Len(t, txns, 1)
	assert.Equal(t, sampleTxns, txns)

	// Query pending
	mockQuery = func(jq *query.QueryJSON) ([]*pldapi.PublicTxWithBinding, error) {
		assert.JSONEq(t, `{
			"limit": 100,
			"eq": [{"field":"nonce","value":12345}],
			"null":[{"field":"transactionHash"}]}`, string(tktypes.JSONString(jq)))
		return sampleTxns, nil
	}
	err = rpcClient.CallRPC(ctx, &txns, "ptx_queryPendingPublicTransactions", query.NewQueryBuilder().
		Equal("nonce", 12345).
		Limit(100).Query())
	require.NoError(t, err)
	require.Len(t, txns, 1)
	assert.Equal(t, sampleTxns, txns)

	// Query missing limit
	err = rpcClient.CallRPC(ctx, &txns, "ptx_queryPublicTransactions", query.NewQueryBuilder().Query())
	require.Regexp(t, "PD012200", err)

	// Query fail
	mockQuery = func(_ *query.QueryJSON) ([]*pldapi.PublicTxWithBinding, error) { return nil, fmt.Errorf("pop") }
	err = rpcClient.CallRPC(ctx, &txns, "ptx_queryPublicTransactions", query.NewQueryBuilder().Limit(100).Query())
	require.Regexp(t, "pop", err)

	// Query by nonce
	mockQuery = func(jq *query.QueryJSON) ([]*pldapi.PublicTxWithBinding, error) {
		assert.JSONEq(t, `{
			"limit": 1,
			"eq": [{"field":"from","value":"`+tx.From.String()+`"},{"field":"nonce","value":"`+tx.Nonce.String()+`"}]
		}`, string(tktypes.JSONString(jq)))
		return sampleTxns, nil
	}
	var txn *pldapi.PublicTxWithBinding
	err = rpcClient.CallRPC(ctx, &txn, "ptx_getPublicTransactionByNonce", tx.From, tx.Nonce)
	require.NoError(t, err)
	assert.Equal(t, sampleTxns[0], txn)

	// Query by nonce err
	mockQuery = func(_ *query.QueryJSON) ([]*pldapi.PublicTxWithBinding, error) { return nil, fmt.Errorf("pop") }
	err = rpcClient.CallRPC(ctx, &txn, "ptx_getPublicTransactionByNonce", tx.From, tx.Nonce)
	require.Regexp(t, "pop", err)

	// Query by hash
	txHash := tktypes.Bytes32(tktypes.RandBytes(32))
	mockGetByHash = func(hash tktypes.Bytes32) (*pldapi.PublicTxWithBinding, error) {
		assert.Equal(t, txHash, hash)
		return tx, nil
	}
	err = rpcClient.CallRPC(ctx, &txn, "ptx_getPublicTransactionByHash", txHash)
	require.NoError(t, err)
	assert.Equal(t, sampleTxns[0], txn)
}

func TestDetailedReceiptRPCsNotFound(t *testing.T) {

	ctx, url, _, done := newTestTransactionManagerWithRPC(t, func(tmc *pldconf.TxManagerConfig, mc *mockComponents) {
		mc.stateMgr.On("GetTransactionStates", mock.Anything, mock.Anything, mock.Anything).
			Return(&pldapi.TransactionStates{None: true}, nil)

		md := componentmocks.NewDomain(t)
		mc.domainManager.On("GetDomainByName", mock.Anything, "domain1").Return(md, nil)
		md.On("GetDomainReceipt", mock.Anything, mock.Anything, mock.Anything).Return(tktypes.RawJSON(`{}`), nil)
	})
	defer done()

	rpcClient, err := rpcclient.NewHTTPClient(ctx, &pldconf.HTTPClientConfig{URL: url})
	require.NoError(t, err)

	var txReceipt *pldapi.TransactionReceiptFull
	err = rpcClient.CallRPC(ctx, &txReceipt, "ptx_getTransactionReceiptFull", uuid.New())
	require.NoError(t, err)
	assert.Nil(t, txReceipt)

	var txStates *pldapi.TransactionStates
	err = rpcClient.CallRPC(ctx, &txStates, "ptx_getStateReceipt", uuid.New())
	require.NoError(t, err)
	assert.Equal(t, &pldapi.TransactionStates{None: true}, txStates)

	var domainReceipt tktypes.RawJSON
	err = rpcClient.CallRPC(ctx, &domainReceipt, "ptx_getDomainReceipt", "domain1", uuid.New())
	require.NoError(t, err)
	assert.JSONEq(t, `{}`, domainReceipt.Pretty())

}

func TestIdentityResolvePassthroughQueries(t *testing.T) {

	ctx, url, _, done := newTestTransactionManagerWithRPC(t,
		func(tmc *pldconf.TxManagerConfig, mc *mockComponents) {
			mc.identityResolver.On("ResolveVerifier", mock.Anything, "lookup1", algorithms.ECDSA_SECP256K1, verifiers.ETH_ADDRESS).
				Return("0x6f4b36e614cf32a20f4c2146d9db4c59a699ea65", nil)
		},
	)
	defer done()

	rpcClient, err := rpcclient.NewHTTPClient(ctx, &pldconf.HTTPClientConfig{URL: url})
	require.NoError(t, err)

	var verifier string
	err = rpcClient.CallRPC(ctx, &verifier, "ptx_resolveVerifier", "lookup1", algorithms.ECDSA_SECP256K1, verifiers.ETH_ADDRESS)
	require.NoError(t, err)
	assert.Equal(t, "0x6f4b36e614cf32a20f4c2146d9db4c59a699ea65", verifier)

}

func TestDebugTransactionStatus(t *testing.T) {

	contractAddress := tktypes.RandAddress()
	txID := uuid.New().String()

	ctx, url, _, done := newTestTransactionManagerWithRPC(t,
		func(tmc *pldconf.TxManagerConfig, mc *mockComponents) {
			mc.privateTxMgr.On("GetTxStatus", mock.Anything, contractAddress.String(), txID).Return(components.PrivateTxStatus{
				TxID:        txID,
				Status:      "pending",
				LatestEvent: "submitted",
				LatestError: "some error message",
			}, nil)
		},
	)
	defer done()

	rpcClient, err := rpcclient.NewHTTPClient(ctx, &pldconf.HTTPClientConfig{URL: url})
	require.NoError(t, err)

	var result components.PrivateTxStatus
	err = rpcClient.CallRPC(ctx, &result, "debug_getTransactionStatus", contractAddress.String(), txID)
	require.NoError(t, err)
	assert.Equal(t, txID, result.TxID)
	assert.Equal(t, "pending", result.Status)
	assert.Equal(t, "submitted", result.LatestEvent)
	assert.Equal(t, "some error message", result.LatestError)

}

func TestQueryPreparedTransactionsNotFound(t *testing.T) {

	ctx, url, _, done := newTestTransactionManagerWithRPC(t)
	defer done()

	rpcClient, err := rpcclient.NewHTTPClient(ctx, &pldconf.HTTPClientConfig{URL: url})
	require.NoError(t, err)

	var pt *pldapi.PreparedTransaction
	err = rpcClient.CallRPC(ctx, &pt, "ptx_getPreparedTransaction", uuid.New())
	require.NoError(t, err)
	require.Nil(t, pt)

	var pts []*pldapi.PreparedTransaction
	err = rpcClient.CallRPC(ctx, &pts, "ptx_queryPreparedTransactions", query.NewQueryBuilder().Limit(10).Query())
	require.NoError(t, err)
	require.Empty(t, pts)

}

func TestPrepareTransactions(t *testing.T) {

	ctx, url, _, done := newTestTransactionManagerWithRPC(t, mockKeyResolutionContextOk(t), func(tmc *pldconf.TxManagerConfig, mc *mockComponents) {
		mc.privateTxMgr.On("HandleNewTx", mock.Anything, mock.MatchedBy(func(tx *components.ValidatedTransaction) bool {
			return tx.Transaction.SubmitMode.V() == pldapi.SubmitModeExternal
		})).Return(nil)
	})
	defer done()

	rpcClient, err := rpcclient.NewHTTPClient(ctx, &pldconf.HTTPClientConfig{URL: url})
	require.NoError(t, err)

	validPublicTx := &pldapi.TransactionInput{
		ABI: abi.ABI{{Type: abi.Function, Name: "doStuff"}},
		TransactionBase: pldapi.TransactionBase{
			Type:           pldapi.TransactionTypePublic.Enum(),
			IdempotencyKey: "tx1",
			From:           "sender1",
			To:             tktypes.RandAddress(),
			Data:           tktypes.RawJSON(`[]`),
		},
	}

	var txID *uuid.UUID
	err = rpcClient.CallRPC(ctx, &txID, "ptx_prepareTransaction", validPublicTx)
	assert.Regexp(t, "PD012225", err)

	var txIDs []uuid.UUID
	err = rpcClient.CallRPC(ctx, &txIDs, "ptx_prepareTransactions", []*pldapi.TransactionInput{validPublicTx})
	assert.Regexp(t, "PD012225", err)

	validPrivateTx := &pldapi.TransactionInput{
		ABI: abi.ABI{{Type: abi.Function, Name: "doStuff"}},
		TransactionBase: pldapi.TransactionBase{
			Type:           pldapi.TransactionTypePrivate.Enum(),
			Domain:         "domain1",
			IdempotencyKey: "tx1",
			From:           "sender1",
			To:             tktypes.RandAddress(),
			Data:           tktypes.RawJSON(`[]`),
		},
	}

	err = rpcClient.CallRPC(ctx, &txID, "ptx_prepareTransaction", validPrivateTx)
	require.NoError(t, err)

	var returnedTX *pldapi.Transaction
	err = rpcClient.CallRPC(ctx, &returnedTX, "ptx_getTransaction", txID)
	require.NoError(t, err)
	require.Equal(t, pldapi.SubmitModeExternal, returnedTX.SubmitMode.V())

}
